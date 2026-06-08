// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"context"
	"fmt"
	"math"

	"github.com/penny-vault/pvbt/asset"
	"github.com/penny-vault/pvbt/data"
	"github.com/penny-vault/pvbt/engine"
	"github.com/penny-vault/pvbt/portfolio"
)

// regimeBranch labels the regime decision and is recorded in the rebalance
// justification.
type regimeBranch string

const (
	regimePanic    regimeBranch = "panic"
	regimeMeltUp   regimeBranch = "melt-up"
	regimeCalm     regimeBranch = "calm"
	regimeSpike    regimeBranch = "spike"
	regimeTrendOn  regimeBranch = "trend-on"
	regimeTrendOff regimeBranch = "trend-off"
)

// regimeDecision records the per-day regime evaluation: which branch fired,
// whether the random forest said "bullish", and what weight (as a fraction of
// LongGross) is targeted at the equity and gold sleeves respectively.
type regimeDecision struct {
	branch       regimeBranch
	mlBullish    bool
	equityWeight float64 // fraction of LongGross to put into top-4
	goldWeight   float64 // fraction of LongGross to put into GLD
}

// checkSignalLong mirrors lsh.py CheckSignal_Long (the +30 min routine). It
// first exits held longs that fell out of the top set (LiquidateNonTopLongsOnly),
// then fetches SPY+VIX daily history, classifies the regime, and allocates the
// top-set equity sleeve plus GLD via safeAllocate. Each sized name's trailing-
// stop high-water mark is initialized off the live 10:00 intraday price. Order
// emission is incremental (Allocate/Liquidate), so names left untouched keep
// their existing positions.
func (s *LongShortHarvest) checkSignalLong(ctx context.Context, eng *engine.Engine, port portfolio.Portfolio, batch *portfolio.Batch) error {
	// LiquidateNonTopLongsOnly (lsh.py:273): exit held long positions that are
	// not SPY/GLD and not in the current top set. Shorts (qty < 0) are left
	// alone; the short sleeve manages them.
	topMembers := assetSet(s.topSet)
	for sym, qty := range port.Holdings() {
		if qty <= 0 {
			continue
		}
		if sym == s.spy || sym == s.gld {
			continue
		}
		if _, ok := topMembers[sym]; ok {
			continue
		}
		if err := batch.Liquidate(ctx, sym); err != nil {
			return fmt.Errorf("liquidate non-top long %s: %w", sym.Ticker, err)
		}
		delete(s.longTrail, sym)
	}

	if len(s.topSet) == 0 {
		return nil
	}

	// QC's CheckSignal_Long calls:
	//   self.History([self.spy], 200, Resolution.Daily)
	//   self.History([self.vix], 100, Resolution.Daily)
	// SPY is a standard equity; QC's History returns DIVIDEND-ADJUSTED
	// closes. We pull data.AdjClose so the spy_5d_ret in the panic branch
	// agrees with QC's calculation.
	// VIX is a CUSTOM PythonData feed (CBOE CSV) which has no dividends, so
	// adjusted == raw. For custom data, QC interprets the bar count as
	// calendar days, yielding ~71 trading bars (100 * 5/7). Match QC's
	// effective window: 71 VIX bars.
	spyDF, err := eng.Fetch(ctx, []asset.Asset{s.spy}, tradingDays(220), []data.Metric{data.AdjClose})
	if err != nil {
		return fmt.Errorf("fetch SPY: %w", err)
	}
	vixDF, err := eng.Fetch(ctx, []asset.Asset{s.vix}, tradingDays(110), []data.Metric{data.MetricClose})
	if err != nil {
		return fmt.Errorf("fetch VIX: %w", err)
	}

	spyClosesRaw := spyDF.Column(s.spy, data.AdjClose)
	vixClosesRaw := vixDF.Column(s.vix, data.MetricClose)

	// CheckSignal_Long fires at +30 min after the open. At that time today's
	// daily SPY bar hasn't closed (close is 4 PM ET), so QC's History returns
	// SPY bars through YESTERDAY's close. pvbt's daily Fetch anchors on the
	// trading-day boundary even during an intraday firing, so it INCLUDES
	// today's daily SPY bar -- drop the last bar to match QC's lag. For VIX,
	// QC's custom feed timestamps the bar at midnight, so the "today" VIX bar
	// IS available at +30 min; we keep it (no drop). Mismatched lag by design:
	// SPY lagged one day, VIX current.
	if len(spyClosesRaw) > 0 {
		spyClosesRaw = spyClosesRaw[:len(spyClosesRaw)-1]
	}

	const vixBarsQCEffective = 71
	if len(spyClosesRaw) < 200 || len(vixClosesRaw) < vixBarsQCEffective {
		return nil
	}

	spyCloses := spyClosesRaw[len(spyClosesRaw)-200:]
	vixCloses := vixClosesRaw[len(vixClosesRaw)-vixBarsQCEffective:]

	decision := s.classifyRegime(vixCloses, spyCloses)
	batch.Annotate("regime", string(decision.branch))
	batch.Annotate("ml_bullish", boolStr(decision.mlBullish))

	// Record the regime intermediates for cross-comparison with QC's
	// classifier on the same SPY/VIX series.
	currentVix := vixCloses[len(vixCloses)-1]
	vixSMA20 := mean(vixCloses[len(vixCloses)-20:])
	vixP80 := percentileValue(vixCloses, 80)
	spyNow := spyCloses[len(spyCloses)-1]
	spySMA50 := mean(spyCloses[len(spyCloses)-50:])
	spySMA200 := mean(spyCloses[len(spyCloses)-200:])
	spy5d := returnNAgo(spyCloses, 4) // QC's spy_5d_ret is the 4-bar return
	batch.Annotate("vix", fmt.Sprintf("%.4f", currentVix))
	batch.Annotate("vix_sma20", fmt.Sprintf("%.4f", vixSMA20))
	batch.Annotate("vix_p80", fmt.Sprintf("%.4f", vixP80))
	batch.Annotate("spy", fmt.Sprintf("%.4f", spyNow))
	batch.Annotate("spy_sma50", fmt.Sprintf("%.4f", spySMA50))
	batch.Annotate("spy_sma200", fmt.Sprintf("%.4f", spySMA200))
	batch.Annotate("spy_5d_ret", fmt.Sprintf("%.6f", spy5d))

	// Equity sleeve: distribute LongGross * equityWeight across the top set.
	equityTotal := s.LongGross * decision.equityWeight

	// Live 10:00 intraday prices for trailing-stop high-water initialization.
	pxs := s.intradayPrices(ctx, eng, s.topSet)

	if equityTotal <= 0 {
		// QC AllocateTop with TW <= 0 liquidates every top-set long and drops
		// its trail state (lsh.py:242-247).
		for _, sym := range s.topSet {
			if port.Position(sym) > 0 {
				if err := batch.Liquidate(ctx, sym); err != nil {
					return fmt.Errorf("liquidate top %s: %w", sym.Ticker, err)
				}
			}
			delete(s.longTrail, sym)
		}
	} else {
		weights := s.allocateTopWeights(equityTotal, decision.mlBullish)
		for i, sym := range s.topSet {
			if i >= len(weights) {
				break
			}
			if err := s.safeAllocate(ctx, port, batch, sym, weights[i]); err != nil {
				return fmt.Errorf("allocate top %s: %w", sym.Ticker, err)
			}
			s.ensureLongTrailState(sym, weights[i], pxs[sym])
		}
		// QC AllocateTop liquidates SPY if invested (defensive; never longed).
		if err := batch.Liquidate(ctx, s.spy); err != nil {
			return fmt.Errorf("liquidate spy: %w", err)
		}
	}

	// GLD sleeve gets the complementary weight (zero liquidates it).
	gldTotal := s.LongGross * decision.goldWeight
	if err := s.safeAllocate(ctx, port, batch, s.gld, gldTotal); err != nil {
		return fmt.Errorf("allocate gld: %w", err)
	}

	// Names dropped from the top set since the previous month are cleaned out
	// of the trail map here as a backstop (the day's LiquidateNonTopLongsOnly
	// already exited them and dropped their state).
	for a := range s.longTrail {
		if _, kept := topMembers[a]; !kept {
			delete(s.longTrail, a)
		}
	}

	return nil
}

// riskCheckLong mirrors lsh.py RiskCheck_Long (the +90 min routine). For each
// held top-set name it advances the trailing-stop state machine off the live
// 11:00 intraday price: stage 0 -> 1 trims to two-thirds of the full target
// weight, stage 1 -> 2 to one-third, stage 2 -> 3 liquidates. Each transition
// resets the high-water mark to the trigger price, matching QC.
func (s *LongShortHarvest) riskCheckLong(ctx context.Context, eng *engine.Engine, port portfolio.Portfolio, batch *portfolio.Batch) error {
	if len(s.longTrail) == 0 {
		return nil
	}

	// Cleanup: drop trail state for names no longer held long (QC pops any
	// position that is not invested or has gone non-positive).
	for sym := range s.longTrail {
		if port.Position(sym) <= 0 {
			delete(s.longTrail, sym)
		}
	}

	// Iterate the top set in its stable (market-cap) order for deterministic
	// emission; QC's stage machine only acts on names in self._top_set.
	pxs := s.intradayPrices(ctx, eng, s.topSet)
	for _, sym := range s.topSet {
		st, ok := s.longTrail[sym]
		if !ok {
			continue
		}
		px := pxs[sym]
		if math.IsNaN(px) || px <= 0 {
			continue
		}
		if px > st.high {
			st.high = px
		}
		if st.high <= 0 {
			continue
		}
		dd := (st.high - px) / st.high

		switch st.stage {
		case 0:
			if dd >= s.LongTrail1 {
				if err := s.safeAllocate(ctx, port, batch, sym, st.targetW*2.0/3.0); err != nil {
					return fmt.Errorf("trail trim %s: %w", sym.Ticker, err)
				}
				st.stage = 1
				st.high = px
			}
		case 1:
			if dd >= s.LongTrail2 {
				if err := s.safeAllocate(ctx, port, batch, sym, st.targetW*1.0/3.0); err != nil {
					return fmt.Errorf("trail trim %s: %w", sym.Ticker, err)
				}
				st.stage = 2
				st.high = px
			}
		case 2:
			if dd >= s.LongTrail3 {
				if err := batch.Liquidate(ctx, sym); err != nil {
					return fmt.Errorf("trail liquidate %s: %w", sym.Ticker, err)
				}
				delete(s.longTrail, sym)
			}
		}
	}

	return nil
}

// classifyRegime walks the QC long-side decision tree using the trailing
// VIX/SPY closes. The branches are tested in priority order; the first match
// wins. The regime decision's equityWeight and goldWeight are expressed as
// fractions of LongGross to keep the multiplier near the call site.
func (s *LongShortHarvest) classifyRegime(vixCloses, spyCloses []float64) regimeDecision {
	mlBullish := false
	if s.model != nil && s.model.trained {
		feats, ok := regimeFeatures(vixCloses, spyCloses)
		if ok {
			mlBullish = s.model.predictBullish(feats)
		}
	}

	currentVix := vixCloses[len(vixCloses)-1]
	vixSMA20 := mean(vixCloses[len(vixCloses)-20:])
	vixP80 := percentileValue(vixCloses, 80)

	spyNow := spyCloses[len(spyCloses)-1]
	spySMA50 := mean(spyCloses[len(spyCloses)-50:])
	spySMA200 := mean(spyCloses[len(spyCloses)-200:])
	// QC's variable is named spy_5d_ret but the formula is closes[-1]/closes[-5]
	// which spans only FOUR bars (positions t-4..t, five elements). Use the
	// 4-bar form here to match QC's panic-branch threshold exactly.
	spy5d := returnNAgo(spyCloses, 4)

	// Branch 1: panic.
	if !math.IsNaN(currentVix) && !math.IsNaN(vixP80) && !math.IsNaN(spy5d) &&
		currentVix > vixP80 && spy5d < -0.03 {
		w := 0.85
		if mlBullish {
			w = 1.0
		}
		return regimeDecision{branch: regimePanic, mlBullish: mlBullish, equityWeight: w, goldWeight: 1.0 - w}
	}

	// Branch 2: melt-up.
	if !math.IsNaN(currentVix) && !math.IsNaN(spySMA50) &&
		currentVix < 13 && spyNow > spySMA50*1.05 {
		return regimeDecision{branch: regimeMeltUp, mlBullish: mlBullish, equityWeight: 0.40, goldWeight: 0.40}
	}

	// Branch 3: calm.
	if !math.IsNaN(vixSMA20) && currentVix > 20 && currentVix < vixSMA20 {
		w := 0.70
		if mlBullish {
			w = 0.85
		}
		return regimeDecision{branch: regimeCalm, mlBullish: mlBullish, equityWeight: w, goldWeight: 1.0 - w}
	}

	// Branch 4: vol spike.
	if !math.IsNaN(vixSMA20) && currentVix > vixSMA20*1.20 {
		return regimeDecision{branch: regimeSpike, mlBullish: mlBullish, equityWeight: 0.0, goldWeight: 0.50}
	}

	// Branch 5: default trend.
	if !math.IsNaN(spySMA200) && spyNow > spySMA200 {
		w := 0.70
		if mlBullish {
			w = 0.90
		}
		return regimeDecision{branch: regimeTrendOn, mlBullish: mlBullish, equityWeight: w, goldWeight: 1.0 - w}
	}

	return regimeDecision{branch: regimeTrendOff, mlBullish: mlBullish, equityWeight: 0.30, goldWeight: 0.50}
}

// allocateTopWeights distributes total across len(s.topSet) names equally,
// applies the ML overweight tilt to the largest member by market cap (when
// bullish), and clamps each to [TopWeightMin, TopWeightMax] with iterative
// renormalization. The returned slice is index-aligned with s.topSet.
func (s *LongShortHarvest) allocateTopWeights(total float64, mlBullish bool) []float64 {
	n := len(s.topSet)
	if n == 0 || total <= 0 {
		return make([]float64, n)
	}

	base := total / float64(n)
	weights := make([]float64, n)
	for i := range weights {
		weights[i] = base
	}

	if mlBullish && s.MLTilt > 0 && n >= 2 {
		// The QC code overweights the largest name by market cap. The top set
		// is already sorted by market cap descending at refresh time, so
		// index 0 is the overweight target.
		overweight := 0
		extra := base * s.MLTilt
		weights[overweight] += extra
		share := extra / float64(n-1)
		for i := range weights {
			if i != overweight {
				weights[i] -= share
			}
		}
	}

	return capAndRenormalize(weights, total, s.TopWeightMin, s.TopWeightMax)
}

// capAndRenormalize clamps each weight to [wmin, wmax] and rebalances the
// residual against the un-clamped slots so the total again equals target. Up
// to ten passes are performed; if the constraints cannot be satisfied the
// best-effort result is returned.
func capAndRenormalize(weights []float64, target, wmin, wmax float64) []float64 {
	w := make([]float64, len(weights))
	copy(w, weights)
	n := len(w)
	if n == 0 {
		return w
	}

	clamp := func() {
		for i := range w {
			if wmin > 0 && w[i] < wmin {
				w[i] = wmin
			}
			if wmax > 0 && w[i] > wmax {
				w[i] = wmax
			}
		}
	}

	clamp()
	for pass := 0; pass < 10; pass++ {
		sum := 0.0
		for _, v := range w {
			sum += v
		}
		diff := target - sum
		if math.Abs(diff) < 1e-8 {
			break
		}

		var adjustable []int
		for i := range w {
			if diff > 0 && (wmax <= 0 || w[i] < wmax-1e-12) {
				adjustable = append(adjustable, i)
			} else if diff < 0 && (wmin <= 0 || w[i] > wmin+1e-12) {
				adjustable = append(adjustable, i)
			}
		}
		if len(adjustable) == 0 {
			break
		}

		incr := diff / float64(len(adjustable))
		for _, i := range adjustable {
			w[i] += incr
		}
		clamp()
	}

	return w
}

// applyLongTrailStage scales a base target weight by the trim factor implied
// by the current trailing-stop stage for that asset. Retained for unit-test
// coverage of the stage-scaling factors used by riskCheckLong.
func (s *LongShortHarvest) applyLongTrailStage(sym asset.Asset, base float64, _ portfolio.Portfolio) float64 {
	st, ok := s.longTrail[sym]
	if !ok {
		return base
	}
	switch st.stage {
	case 1:
		return base * 2.0 / 3.0
	case 2:
		return base * 1.0 / 3.0
	case 3:
		return 0
	default:
		return base
	}
}

// ensureLongTrailState creates the per-position trail entry on first inclusion
// (high-water seeded to the current intraday price) or refreshes the recorded
// full target weight when sizing changes, leaving the high-water mark and stage
// intact. Mirrors QC's _ensure_long_trail_state (lsh.py:224).
func (s *LongShortHarvest) ensureLongTrailState(sym asset.Asset, target, px float64) {
	if target <= 0 {
		delete(s.longTrail, sym)
		return
	}
	if math.IsNaN(px) || px <= 0 {
		return
	}
	st, ok := s.longTrail[sym]
	if !ok {
		s.longTrail[sym] = &longTrailState{high: px, stage: 0, targetW: target}
		return
	}
	st.targetW = target
}

func assetSet(assets []asset.Asset) map[asset.Asset]struct{} {
	out := make(map[asset.Asset]struct{}, len(assets))
	for _, a := range assets {
		out[a] = struct{}{}
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
