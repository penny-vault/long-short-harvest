// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/penny-vault/pvbt/asset"
	"github.com/penny-vault/pvbt/data"
	"github.com/penny-vault/pvbt/engine"
	"github.com/penny-vault/pvbt/portfolio"
	"github.com/rs/zerolog"
)

// hurstWindows mirrors the QC `n_list`: six lookbacks summed into the
// composite score. The duplicate `10` is intentional; it weights the very
// short window twice in the average.
var hurstWindows = []int{10, 10, 40, 60, 90, 100}

// shortCandidate carries the per-name score and entry telemetry through the
// rebalance pipeline.
type shortCandidate struct {
	asset asset.Asset
	score float64
	close float64
	atr20 float64
}

// computeShortSleeve maintains the short book. It runs the ATR-distance
// stop check daily and a full rebalance on Mondays, picking the top-N
// trend-exhaustion candidates by composite Hurst-like score.
func (s *LongShortHarvest) computeShortSleeve(ctx context.Context, eng *engine.Engine, port portfolio.Portfolio, batch *portfolio.Batch, today time.Time, plan *weightPlan) error {
	// Daily ATR-stop sweep on existing shorts. Stops emit zero-weight in
	// the plan so the position is liquidated on this day's rebalance.
	covered := s.applyShortATRStops(port)
	if len(covered) > 0 {
		batch.Annotate("short_covered", strconv.FormatInt(int64(len(covered)), 10))
		for sym := range covered {
			plan.add(sym, 0)
		}
	}

	// Off-Monday: pvbt's RebalanceTo liquidates any name not in the
	// allocation, so existing shorts MUST appear in the plan or they get
	// covered Tuesday morning. Emit each at its CURRENT REALIZED weight
	// so RebalanceTo computes a zero-dollar adjustment and no order
	// fires -- the share count stays constant and the realized weight
	// drifts with price, matching QC's "shares held between rebalances"
	// semantic. This relies on pvbt MaxLeverage being an entry-time gate
	// only (post-pvbt-team change); adverse drift no longer triggers
	// proactive liquidation.
	holdings := port.Holdings()
	portValue := port.Value()
	for sym, qty := range holdings {
		if qty >= 0 {
			continue
		}
		if _, gone := covered[sym]; gone {
			continue
		}
		if _, ok := s.shortEntry[sym]; !ok {
			continue
		}
		realizedWeight := math.NaN()
		if portValue > 0 {
			realizedWeight = port.PositionValue(sym) / portValue
		}
		if math.IsNaN(realizedWeight) || realizedWeight >= 0 {
			continue
		}
		plan.add(sym, realizedWeight)
	}

	if today.Weekday() != time.Monday {
		return nil
	}

	// Monday rebalance: rank the candidate pool, keep top-N above threshold,
	// short them equally to ShortGross.
	if len(s.activeShorts) == 0 {
		return nil
	}

	candidates, err := s.rankShortCandidates(ctx, eng)
	if err != nil {
		return fmt.Errorf("rank short candidates: %w", err)
	}

	picked := s.selectShortCandidates(candidates)

	batch.Annotate("short_universe", strconv.Itoa(len(s.activeShorts)))
	batch.Annotate("short_ranked", strconv.Itoa(len(candidates)))
	batch.Annotate("short_picked", strconv.Itoa(len(picked)))
	if len(candidates) > 0 {
		topScore := candidates[0].score
		for _, c := range candidates {
			if c.score > topScore {
				topScore = c.score
			}
		}
		batch.Annotate("short_top_score", strconv.FormatFloat(topScore, 'f', 4, 64))
	}
	if len(picked) > 0 {
		names := make([]string, 0, len(picked))
		for _, c := range picked {
			names = append(names, c.asset.Ticker)
		}
		batch.Annotate("short_picked_names", fmt.Sprint(names))
	}

	// Drop weights for the previous shorts that did not make this week's
	// selection: omit them from the plan and the rebalance liquidates.
	keep := make(map[asset.Asset]struct{}, len(picked))
	for _, p := range picked {
		keep[p.asset] = struct{}{}
	}
	for sym := range s.shortEntry {
		if _, ok := keep[sym]; !ok {
			// Mark for cover by ensuring no plan weight is added.
			delete(s.shortEntry, sym)
			plan.add(sym, 0)
		}
	}

	if len(picked) == 0 {
		return nil
	}

	weight := -math.Abs(s.ShortGross) / float64(len(picked))
	for _, c := range picked {
		plan.add(c.asset, weight)
		if existing, ok := s.shortEntry[c.asset]; ok {
			existing.targetWeight = weight
		} else {
			s.shortEntry[c.asset] = &shortEntryState{
				entryPrice:   c.close,
				entryATR:     c.atr20,
				targetWeight: weight,
			}
		}
	}
	plan.note(fmt.Sprintf("short: %d picked", len(picked)))
	return nil
}

// applyShortATRStops covers any open short whose mark has run against the
// trade by at least StopATR * entry-ATR. Returns the set of assets covered
// so the caller can avoid re-emitting them.
func (s *LongShortHarvest) applyShortATRStops(port portfolio.Portfolio) map[asset.Asset]struct{} {
	out := make(map[asset.Asset]struct{})
	for sym, st := range s.shortEntry {
		qty := port.Position(sym)
		if qty >= 0 {
			delete(s.shortEntry, sym)
			continue
		}
		px := s.lastKnownPrice(port, sym)
		if math.IsNaN(px) || px <= 0 || st.entryATR <= 0 {
			continue
		}
		if (px - st.entryPrice) >= s.StopATR*st.entryATR {
			out[sym] = struct{}{}
			delete(s.shortEntry, sym)
		}
	}
	return out
}

// rankShortCandidates fetches per-name OHLC history for each candidate and
// computes the Hurst-like composite score plus the extension/momentum gates.
// Candidates failing the gates are dropped; the remainder are returned
// unsorted with score, close, and entry ATR populated.
func (s *LongShortHarvest) rankShortCandidates(ctx context.Context, eng *engine.Engine) ([]shortCandidate, error) {
	if len(s.activeShorts) == 0 {
		return nil, nil
	}

	df, err := eng.Fetch(ctx, s.activeShorts, tradingDays(s.LookbackBars+10), []data.Metric{
		data.MetricHigh, data.MetricLow, data.MetricClose,
	})
	if err != nil {
		return nil, fmt.Errorf("fetch short history: %w", err)
	}

	today := eng.CurrentDate()
	dbgDate := isDebugDate(today)
	debugTickers := map[string]struct{}{
		"TWLO": {}, "ESPR": {}, "SKX": {}, "KLAC": {},
	}

	if dbgDate {
		tickers := make([]string, 0, len(s.activeShorts))
		hits := map[string]bool{}
		for _, a := range s.activeShorts {
			tickers = append(tickers, a.Ticker)
			if _, ok := debugTickers[a.Ticker]; ok {
				hits[a.Ticker] = true
			}
		}
		zerolog.Ctx(ctx).Info().
			Str("date", today.Format("2006-01-02")).
			Int("count", len(tickers)).
			Bool("twlo_in", hits["TWLO"]).
			Bool("espr_in", hits["ESPR"]).
			Bool("skx_in", hits["SKX"]).
			Bool("klac_in", hits["KLAC"]).
			Msg("DBG_LSH activeShorts membership probe")
	}

	// Exclude any name currently in the long top-4 from short candidacy.
	// Without this, on Mondays when a top-4 name has just gapped sharply
	// higher (e.g. GOOGL after a +16% earnings beat), its score can clear
	// 0.85 and the short sleeve takes a -0.6 position in a name the long
	// sleeve is simultaneously holding +0.225 of, producing a net short for
	// a few days until the next Monday rebalance. QC's 2015 trade log shows
	// no name was ever simultaneously long and short, so mirror that.
	longExcluded := assetSet(s.topSet)

	out := make([]shortCandidate, 0, len(s.activeShorts))
	for _, sym := range s.activeShorts {
		if _, isLong := longExcluded[sym]; isLong {
			continue
		}
		highsRaw := df.Column(sym, data.MetricHigh)
		lowsRaw := df.Column(sym, data.MetricLow)
		closesRaw := df.Column(sym, data.MetricClose)

		// Drop the most recent bar so the Hurst/ATR/SMA chain sees the same
		// window QC's Rebalance_Short sees: it runs 30 minutes after Monday's
		// open and History() returns through Friday's close. Computing on
		// today's bar effectively peeks at the post-rebalance price, so trim
		// it off and slice to exactly LookbackBars trailing bars.
		if len(closesRaw) < 2 {
			continue
		}
		end := len(closesRaw) - 1
		start := end - s.LookbackBars
		if start < 0 {
			start = 0
		}
		highs := highsRaw[start:end]
		lows := lowsRaw[start:end]
		closes := closesRaw[start:end]

		score, ok := s.scoreCandidate(highs, lows, closes)
		_, dbgTicker := debugTickers[sym.Ticker]
		dbgThis := dbgDate && dbgTicker
		if !ok {
			if dbgThis {
				zerolog.Ctx(ctx).Info().
					Str("symbol", sym.Ticker).
					Str("date", today.Format("2006-01-02")).
					Int("bars", len(closes)).
					Msg("DBG_LSH scoreCandidate returned !ok")
			}
			continue
		}

		closeNow := closes[len(closes)-1]
		atr20 := atr(highs, lows, closes, 20)
		if math.IsNaN(atr20) || atr20 <= 0 {
			continue
		}

		smaWindow := s.SMALen
		if smaWindow <= 0 || smaWindow > len(closes) {
			smaWindow = len(closes)
		}
		sma := mean(closes[len(closes)-smaWindow:])
		if math.IsNaN(sma) {
			continue
		}

		ext := closeNow - sma
		// QC's `df["close"].iloc[-5]` is the 5th-from-last bar = 4 positions
		// before closes[len-1]. Use closes[len-5] in Go to match exactly.
		mom := math.NaN()
		if len(closes) >= 5 {
			mom = closeNow - closes[len(closes)-5]
		}

		if dbgThis {
			s.logShortDiagnostic(ctx, sym.Ticker, today, highs, lows, closes, score, closeNow, atr20, sma, ext, mom)
		}

		if ext <= s.ExtK*atr20 {
			continue
		}

		if len(closes) < 5 {
			continue
		}
		if mom <= s.MomK*atr20 {
			continue
		}

		out = append(out, shortCandidate{
			asset: sym,
			score: score,
			close: closeNow,
			atr20: atr20,
		})
	}

	if dbgDate {
		// Log top-5 scorers (after gates) for cross-system comparison.
		ranked := make([]shortCandidate, len(out))
		copy(ranked, out)
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
		top := ranked[:min(5, len(ranked))]
		ev := zerolog.Ctx(ctx).Info()
		for i, c := range top {
			ev = ev.Str(fmt.Sprintf("rank%d_ticker", i+1), c.asset.Ticker).
				Float64(fmt.Sprintf("rank%d_score", i+1), c.score).
				Float64(fmt.Sprintf("rank%d_close", i+1), c.close).
				Float64(fmt.Sprintf("rank%d_atr20", i+1), c.atr20)
		}
		ev.Int("passed_gates", len(out)).Msg("DBG_LSH top-5 short scorers 2020-05-11")
	}

	return out, nil
}

// isDebugDate returns true on the specific Mondays we're cross-checking
// against the QC reference for the 2015-only run.
func isDebugDate(t time.Time) bool {
	switch t.Format("2006-01-02") {
	case "2015-03-02", "2015-03-23", "2015-04-20", "2015-07-27", "2015-08-03", "2015-10-26", "2020-05-11":
		return true
	}
	return false
}

// logShortDiagnostic emits one log line per debug-ticker hit so the
// Hurst-like calc chain can be compared against the QC reference.
func (s *LongShortHarvest) logShortDiagnostic(ctx context.Context, ticker string, today time.Time, highs, lows, closes []float64, score, closeNow, atr20, sma, ext, mom float64) {
	w := len(closes)
	if w < 100 {
		return
	}
	tail3 := []float64{closes[w-3], closes[w-2], closes[w-1]}

	hvals := make([]float64, 0, len(hurstWindows))
	for _, n := range hurstWindows {
		bump := 0.01 + 0.0002*float64(n)
		hv := hurstLike(highs, lows, closes, n, bump)
		if !math.IsNaN(hv) {
			hvals = append(hvals, hv)
		}
	}
	havg := mean(hvals)
	agree := 0
	for _, h := range hvals {
		if h > 0.6 {
			agree++
		}
	}

	a100 := atr(highs, lows, closes, 100)
	hi100 := math.Inf(-1)
	lo100 := math.Inf(1)
	for i := w - 100; i < w; i++ {
		if highs[i] > hi100 {
			hi100 = highs[i]
		}
		if lows[i] < lo100 {
			lo100 = lows[i]
		}
	}
	span100 := hi100 - lo100
	hRaw100 := math.NaN()
	if a100 > 0 && span100 > 0 {
		hRaw100 = (math.Log(span100) - math.Log(a100)) / math.Log(100.0)
	}
	hFinal100 := hurstLike(highs, lows, closes, 100, 0.01+0.0002*100)

	// Match QC's iloc[-5] semantics: 5th-from-end = 4 positions before the
	// last bar. In Go: closes[len-5].
	close5 := math.NaN()
	if w >= 5 {
		close5 = closes[w-5]
	}

	zerolog.Ctx(ctx).Info().
		Str("symbol", ticker).
		Str("date", today.Format("2006-01-02")).
		Int("bars", w).
		Floats64("close_tail3", tail3).
		Float64("atr20", atr20).
		Float64("sma195", sma).
		Float64("n100_atr", a100).
		Float64("n100_span", span100).
		Float64("n100_h_raw", hRaw100).
		Float64("n100_h_final", hFinal100).
		Floats64("hvals", hvals).
		Float64("havg", havg).
		Int("agree", agree).
		Float64("score", score).
		Bool("ext_ok", ext > s.ExtK*atr20).
		Bool("mom_ok", mom > s.MomK*atr20).
		Float64("close_now", closeNow).
		Float64("close_5", close5).
		Float64("ext", ext).
		Float64("mom", mom).
		Msg("DBG_LSH")
}

// scoreCandidate computes the composite Hurst-like score for a single name.
// Returns the score and true when at least four of the six windows produced a
// usable Hurst measure, matching the QC `len(hvals) < 4` guard.
func (s *LongShortHarvest) scoreCandidate(highs, lows, closes []float64) (float64, bool) {
	maxN := 0
	for _, n := range hurstWindows {
		if n > maxN {
			maxN = n
		}
	}
	if len(closes) < maxN+6 {
		return 0, false
	}

	hvals := make([]float64, 0, len(hurstWindows))
	for _, n := range hurstWindows {
		bump := 0.01 + 0.0002*float64(n)
		hv := hurstLike(highs, lows, closes, n, bump)
		if !math.IsNaN(hv) {
			hvals = append(hvals, hv)
		}
	}
	if len(hvals) < 4 {
		return 0, false
	}

	avg := mean(hvals)
	agree := 0
	for _, h := range hvals {
		if h > 0.6 {
			agree++
		}
	}
	score := avg + 0.02*float64(max0(agree-3))
	return score, true
}

// selectShortCandidates filters by the score threshold, sorts descending, and
// returns the top-N picks.
func (s *LongShortHarvest) selectShortCandidates(candidates []shortCandidate) []shortCandidate {
	filtered := make([]shortCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.score >= s.ScoreThreshold {
			filtered = append(filtered, c)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].score > filtered[j].score
	})

	n := s.TopN
	if n <= 0 {
		n = 1
	}
	if len(filtered) < n {
		n = len(filtered)
	}
	return filtered[:n]
}

// positionWeight returns the current short weight for the asset (negative
// for short positions). NaN if the portfolio value is non-positive.
func positionWeight(port portfolio.Portfolio, sym asset.Asset) float64 {
	v := port.Value()
	if v <= 0 {
		return math.NaN()
	}
	return port.PositionValue(sym) / v
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}
