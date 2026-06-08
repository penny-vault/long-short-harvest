// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/penny-vault/pvbt/asset"
	"github.com/penny-vault/pvbt/data"
	"github.com/penny-vault/pvbt/engine"
	"github.com/rs/zerolog"
)

// monthKey encodes year+month into a single int for cheap month-change tests.
func monthKey(t time.Time) int { return t.Year()*100 + int(t.Month()) }

// maybeBootstrapLongUniverse runs at the START of the 10:00 slot. It populates
// the long top_set the first time Compute runs so Day-1 has long positions --
// QC's FineSelection runs on the first trading day BEFORE CheckSignal_Long
// fires, so QC's Day-1 allocation already has top_set populated.
//
// The SHORT universe is NOT refreshed here. QC reassigns self._active daily in
// FineSelection, but the only consumer is Rebalance_Short, which fires solely
// on Mondays; refreshing it once on Monday immediately before the rebalance is
// behaviorally identical and avoids the expensive daily ~1800-name universe
// scan (the dominant backtest cost). See refreshShortUniverse's call site in
// Compute. On subsequent month-changes the long top_set is rotated at the END
// of the 10:00 slot (maybeRefreshLongUniverse) so today's allocation still uses
// last month's top_set and the new one takes effect tomorrow.
func (s *LongShortHarvest) maybeBootstrapLongUniverse(ctx context.Context, eng *engine.Engine, today time.Time) error {
	if len(s.topSet) == 0 {
		if err := s.refreshLongUniverse(ctx, eng, today); err != nil {
			return fmt.Errorf("bootstrap long universe: %w", err)
		}
		s.lastUniverseKey = monthKey(today)
	}
	return nil
}

// maybeRefreshLongUniverse runs at the END of Compute, AFTER the long sleeve
// has used s.topSet for today. The newly selected top_set becomes effective
// on the NEXT trading day -- matching QC's behavior where FineSelection fires
// at month-start but the new top_set isn't seen by CheckSignal_Long until the
// following day's run.
func (s *LongShortHarvest) maybeRefreshLongUniverse(ctx context.Context, eng *engine.Engine, today time.Time) error {
	if monthKey(today) == s.lastUniverseKey {
		return nil
	}
	if err := s.refreshLongUniverse(ctx, eng, today); err != nil {
		return fmt.Errorf("refresh long universe: %w", err)
	}
	s.lastUniverseKey = monthKey(today)
	return nil
}

// maybeRetrainModel runs at the END of Compute, AFTER both sleeves have used
// the existing s.model. The newly trained model becomes effective on the NEXT
// trading day, matching QC's TrainModel schedule (60 min after open, after the
// 30-min CheckSignal_Long allocation has already fired).
func (s *LongShortHarvest) maybeRetrainModel(ctx context.Context, eng *engine.Engine, today time.Time) {
	if monthKey(today) == s.lastRetrainKey {
		return
	}
	model, err := s.retrainRegimeModel(ctx, eng)
	s.lastRetrainKey = monthKey(today)
	if model != nil {
		// DBG_TRAIN: summary stats per training call.
		zerolog.Ctx(ctx).Info().
			Str("date", today.Format("2006-01-02")).
			Int("vix_len", model.trainVixLen).
			Int("spy_len", model.trainSpyLen).
			Int("n_samples", model.trainSamples).
			Int("zeros", model.trainZeros).
			Int("ones", model.trainOnes).
			Msg("DBG_TRAIN")
		// DBG_TRAIN_LABELS: compact label sequence so we can diff the
		// exact y array against QC's.
		labelStr := make([]byte, len(model.trainLabels))
		for i, v := range model.trainLabels {
			if v == 1 {
				labelStr[i] = '1'
			} else {
				labelStr[i] = '0'
			}
		}
		zerolog.Ctx(ctx).Info().
			Str("date", today.Format("2006-01-02")).
			Str("labels", string(labelStr)).
			Msg("DBG_TRAIN_LABELS")
		// DBG_TRAIN_ROW: first and last sample feature vectors for spot-check.
		formatRow := func(r []float64) string {
			b := make([]byte, 0, len(r)*10)
			for i, v := range r {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, []byte(fmt.Sprintf("%.6f", v))...)
			}
			return string(b)
		}
		zerolog.Ctx(ctx).Info().
			Str("date", today.Format("2006-01-02")).
			Str("tag", "first").
			Str("feats", formatRow(model.trainFirstRow)).
			Int("label", model.trainFirstLabel).
			Msg("DBG_TRAIN_ROW")
		zerolog.Ctx(ctx).Info().
			Str("date", today.Format("2006-01-02")).
			Str("tag", "last").
			Str("feats", formatRow(model.trainLastRow)).
			Int("label", model.trainLastLabel).
			Msg("DBG_TRAIN_ROW")
	}
	if err != nil {
		// Training failures are non-fatal: the regime model simply stays at
		// its previous state (or untrained) and the rule-based regime takes
		// over for the month.
		return
	}
	s.model = model
}

// candidate is an internal record used during monthly universe ranking.
type candidate struct {
	asset     asset.Asset
	marketCap float64
	dollarVol float64
}

// universeCandidates is the shared pre-filter for both sleeves. It:
//   - resolves USTradable membership at `today`
//   - fetches MarketCap and a window of close+volume
//   - filters: market cap >= 1B, prior close >= 5, dollar volume >= 20M,
//     >= MinHistoryDays of contiguous price history
//
// The returned slice is unsorted; callers rank it by their own key.
func (s *LongShortHarvest) universeCandidates(ctx context.Context, eng *engine.Engine, today time.Time) ([]candidate, error) {
	members := s.usTradable.Assets(today)
	if len(members) == 0 {
		return nil, nil
	}

	// Market cap snapshot at the rebalance date (point-in-time).
	mcapDF, err := eng.FetchAt(ctx, members, today, []data.Metric{data.MarketCap})
	if err != nil {
		return nil, fmt.Errorf("fetch market cap: %w", err)
	}

	// Recent close/volume window for liquidity ranking and dollar-volume gate.
	// 21-day window ~ 1 month of history, sufficient for an average dollar
	// volume estimate.
	priceDF, err := eng.Fetch(ctx, members, tradingDays(30), []data.Metric{data.MetricClose, data.Volume})
	if err != nil {
		return nil, fmt.Errorf("fetch close/volume: %w", err)
	}

	// Long-window probe used as the "min history" filter: if the asset has a
	// non-NaN close MinHistoryDays back, it has been listed long enough.
	historyProbeDays := s.MinHistoryDays
	if historyProbeDays <= 0 {
		historyProbeDays = 252
	}
	historyDF, err := eng.Fetch(ctx, members, tradingDays(historyProbeDays+5), []data.Metric{data.MetricClose})
	if err != nil {
		return nil, fmt.Errorf("fetch history probe: %w", err)
	}
	probeTimes := historyDF.Times()

	var probeIdx int
	if len(probeTimes) > historyProbeDays {
		probeIdx = len(probeTimes) - historyProbeDays
	}

	out := make([]candidate, 0, len(members))
	closeTimes := priceDF.Times()
	if len(closeTimes) == 0 {
		return out, nil
	}
	lastIdx := len(closeTimes) - 1

	dbg := isDebugDate(today)

	for _, m := range members {
		isTarget := dbg && (m.Ticker == "ESPR" || m.Ticker == "SKX" || m.Ticker == "KLAC")

		mcap := mcapDF.Value(m, data.MarketCap)
		if math.IsNaN(mcap) || mcap < 1_000_000_000 {
			if isTarget {
				zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Float64("mcap", mcap).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: failed mcap gate")
			}
			continue
		}

		closeCol := priceDF.Column(m, data.MetricClose)
		volCol := priceDF.Column(m, data.Volume)
		if len(closeCol) == 0 || len(volCol) == 0 {
			if isTarget {
				zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Int("close_len", len(closeCol)).Int("vol_len", len(volCol)).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: missing price/vol columns")
			}
			continue
		}

		lastClose := closeCol[lastIdx]
		if math.IsNaN(lastClose) || lastClose < 5 {
			if isTarget {
				zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Float64("close", lastClose).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: failed price gate")
			}
			continue
		}

		// QC ranks by FineFundamental.DollarVolume which is the most recent
		// single-day dollar volume (close * volume). Match that exactly so a
		// high-volume spike day puts a name into the top-150 the same way it
		// does in the reference. Using a 20-day average smooths these spikes
		// out and excludes names like TWLO that would otherwise be picked.
		lastVol := volCol[lastIdx]
		if math.IsNaN(lastVol) {
			if isTarget {
				zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: NaN vol")
			}
			continue
		}
		dollarVol := lastClose * lastVol
		if dollarVol < 20_000_000 {
			if isTarget {
				zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Float64("dvol", dollarVol).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: failed dvol gate")
			}
			continue
		}

		// Min-history gate: require a non-NaN close at probeIdx.
		probeCol := historyDF.Column(m, data.MetricClose)
		if probeIdx >= len(probeCol) {
			if isTarget {
				zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Int("probe_len", len(probeCol)).Int("probe_idx", probeIdx).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: probe column too short")
			}
			continue
		}
		if math.IsNaN(probeCol[probeIdx]) {
			if isTarget {
				zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Int("probe_idx", probeIdx).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: NaN at probe index (insufficient history)")
			}
			continue
		}

		if isTarget {
			zerolog.Ctx(ctx).Info().Str("ticker", m.Ticker).Float64("mcap", mcap).Float64("dvol", dollarVol).Str("date", today.Format("2006-01-02")).Msg("DBG_LSH universe filter: PASSED, in candidate set")
		}

		out = append(out, candidate{
			asset:     m,
			marketCap: mcap,
			dollarVol: dollarVol,
		})
	}

	return out, nil
}

// refreshLongUniverse picks the top-4 names by market cap from the gated
// candidate pool and updates the strategy's tracked top set. Cleared trail
// state for names that fall out is dropped on the floor; the trail map is
// pruned later in the long-sleeve risk check.
//
// Composition rules (match QC's monthly _top_set verbatim):
//
//  1. **If GOOGL is in pvbt's top-4** by mcap: insert GOOG (Alphabet's other
//     share class) immediately after GOOGL with the same mcap, then drop
//     any BRK/B from candidates. This mirrors QC's GOOCV+GOOG dual-class
//     allocation in months where Alphabet outranks Berkshire (Feb-Mar,
//     May-Dec).
//  2. **Else if BRK/B is in pvbt's top-4** by mcap: replace BRK/B with
//     BRK/A. BRK/A's price is ~$220k/share, so a 15.75% allocation on a
//     ~$100k portfolio rounds to 0 shares -- matching QC's Jan/Apr
//     BRKA-in-top-4-but-doesn't-trade behavior.
//  3. **Else** (neither in top-4): no augmentation.
//
// Pvbt's data has BRK/A and GOOG as known assets but with NaN MarketCap
// (Sharadar consolidates the company mcap onto BRK/B and GOOGL respectively).
// The augmentation gives the missing siblings the same mcap as their twin so
// they enter the ranking, then top-4 is sliced from the augmented list.
func (s *LongShortHarvest) refreshLongUniverse(ctx context.Context, eng *engine.Engine, today time.Time) error {
	candidates, err := s.universeCandidates(ctx, eng, today)
	if err != nil {
		return err
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].marketCap > candidates[j].marketCap
	})

	// Identify top-4 membership for the rule decision.
	googlInTop4 := false
	brkBInTop4 := false
	limit := 4
	if len(candidates) < limit {
		limit = len(candidates)
	}
	for i := 0; i < limit; i++ {
		switch candidates[i].asset.Ticker {
		case "GOOGL":
			googlInTop4 = true
		case "BRK/B":
			brkBInTop4 = true
		}
	}

	augmented := make([]candidate, 0, len(candidates)+2)
	for _, c := range candidates {
		switch c.asset.Ticker {
		case "BRK/B":
			if googlInTop4 {
				// Alphabet doubling will fill the slot; skip BRK.
				continue
			}
			if s.brkA.Ticker != "" {
				augmented = append(augmented, candidate{
					asset:     s.brkA,
					marketCap: c.marketCap,
					dollarVol: c.dollarVol,
				})
			}
			// Drop original BRK/B (replaced by BRK/A which rounds to 0).
		case "BRK/A":
			// Pvbt's BRK/A has NaN mcap so it never reaches here naturally;
			// guard anyway.
			continue
		case "GOOGL":
			augmented = append(augmented, c)
			if googlInTop4 && s.goog.Ticker != "" {
				augmented = append(augmented, candidate{
					asset:     s.goog,
					marketCap: c.marketCap,
					dollarVol: c.dollarVol,
				})
			}
			_ = brkBInTop4 // brkBInTop4 only needed for BRK branch
		default:
			augmented = append(augmented, c)
		}
	}

	count := 4
	if len(augmented) < count {
		count = len(augmented)
	}

	s.topSet = s.topSet[:0]
	for i := 0; i < count; i++ {
		s.topSet = append(s.topSet, augmented[i].asset)
	}
	s.topSetMonth = today.Month()
	s.topSetYear = today.Year()

	return nil
}

// refreshShortUniverse picks the top-MaxUniverse names by trailing dollar
// volume from the gated candidate pool, recording them as the active short
// candidate pool until next month-end.
func (s *LongShortHarvest) refreshShortUniverse(ctx context.Context, eng *engine.Engine, today time.Time) error {
	candidates, err := s.universeCandidates(ctx, eng, today)
	if err != nil {
		return err
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].dollarVol > candidates[j].dollarVol
	})

	count := s.MaxUniverse
	if count <= 0 {
		count = 150
	}
	if len(candidates) < count {
		count = len(candidates)
	}

	s.activeShorts = s.activeShorts[:0]
	for i := 0; i < count; i++ {
		s.activeShorts = append(s.activeShorts, candidates[i].asset)
	}
	s.activeShortMonth = today.Month()
	s.activeShortYear = today.Year()
	return nil
}

// retrainRegimeModel pulls the deepest available SPY+VIX history and fits a
// fresh random-forest classifier. Returns nil when the data is too short to
// produce a usable model; the caller should fall back to the rule-based
// regime in that case.
func (s *LongShortHarvest) retrainRegimeModel(ctx context.Context, eng *engine.Engine) (*regimeModel, error) {
	// QC's TrainModel does:
	//   self.History([self.vix], 800, Resolution.Daily)
	//   self.History([self.spy], 800, Resolution.Daily)
	// SPY (standard equity) returns 800 trading bars. VIX (custom CBOE
	// feed) returns 800 CALENDAR days, which yields 549 trading bars (per
	// the DBG_TRAIN logs). The training loop walks the SPY index but
	// `vix_closes[:i]` caps at 549 once i exceeds that, so VIX features
	// for i > 549 are constant. We replicate both window sizes exactly.
	// SPY uses ADJUSTED close so the forward-return label
	// `spy[i+21]/spy[i] > 1.02` agrees with QC's adjusted-data labels.
	const trainSpyBarsQC = 800
	const trainVixBarsQC = 549
	// Pull a wide enough window to guarantee >=800 SPY bars after slicing.
	// 900 trading days approx ≈ 1290 calendar days; even with holidays
	// the bar count comfortably exceeds 800.
	spyDF, err := eng.Fetch(ctx, []asset.Asset{s.spy}, tradingDays(900), []data.Metric{data.AdjClose})
	if err != nil {
		return nil, fmt.Errorf("fetch spy: %w", err)
	}
	vixDF, err := eng.Fetch(ctx, []asset.Asset{s.vix}, tradingDays(900), []data.Metric{data.MetricClose})
	if err != nil {
		return nil, fmt.Errorf("fetch vix: %w", err)
	}

	spyCloses := spyDF.Column(s.spy, data.AdjClose)
	vixCloses := vixDF.Column(s.vix, data.MetricClose)
	if len(spyCloses) > trainSpyBarsQC {
		spyCloses = spyCloses[len(spyCloses)-trainSpyBarsQC:]
	}
	if len(vixCloses) > trainVixBarsQC {
		vixCloses = vixCloses[len(vixCloses)-trainVixBarsQC:]
	}

	// Do NOT align spy/vix lengths -- QC's training loop intentionally uses
	// different lengths (800 SPY, 549 VIX), with the loop's
	// vix_closes[:i] indexer capping past len(vix). trainRegimeModel
	// replicates that capping behavior.
	model, err := trainRegimeModel(vixCloses, spyCloses, 504)
	if err != nil {
		return nil, err
	}
	return model, nil
}

// alignByLength trims the longer of two close series so both have the same
// length. The training procedure consumes them as equal-length aligned
// vectors; an exact timestamp join is more correct but the FRED VIX series
// and the SPY price series share the US trading calendar and the longer of
// the two is at most one bar ahead.
func alignByLength(a, b []float64) ([]float64, []float64) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if len(a) > n {
		a = a[len(a)-n:]
	}
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return a, b
}
