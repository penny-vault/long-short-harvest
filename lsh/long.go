// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"context"
	"fmt"
	"math"
	"time"

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

// computeLongSleeve fetches SPY+VIX history, runs the regime classifier,
// allocates the top-4 names plus GLD, and applies the trailing-stop state
// machine to long positions.
func (s *LongShortHarvest) computeLongSleeve(ctx context.Context, eng *engine.Engine, port portfolio.Portfolio, batch *portfolio.Batch, today time.Time, plan *weightPlan) error {
	if len(s.topSet) == 0 {
		return nil
	}

	// QC's CheckSignal_Long calls:
	//   self.History([self.spy], 200, Resolution.Daily)
	//   self.History([self.vix], 100, Resolution.Daily)
	// SPY is a standard equity; QC's History returns DIVIDEND-ADJUSTED
	// closes. We pull data.AdjClose so the 21-day forward return labels
	// in ML training and the spy_5d_ret in the panic branch agree with
	// QC's calculations.
	// VIX is a CUSTOM PythonData feed (CBOE CSV) which has no dividends,
	// so adjusted == raw. For custom data, QC interprets the bar count
	// as calendar days, yielding ~71 trading bars (100 * 5/7). Match
	// QC's effective window: 71 VIX bars.
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

	// QC's CheckSignal_Long fires at +30 min after market open. At that
	// time, today's daily SPY bar hasn't closed (close is at 4 PM ET), so
	// History returns SPY bars through YESTERDAY's close. For VIX, QC
	// loads it as a custom PythonData feed with bar.Time = midnight of
	// the date, so the "today" VIX bar IS available at +30 min and shows
	// up in History. Mismatched lag: SPY lagged 1 day, VIX current. Drop
	// the last SPY bar to match.
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
	plan.note(fmt.Sprintf("long: %s ml=%v", decision.branch, decision.mlBullish))

	// Record the regime intermediates for cross-comparison with QC's
	// classifier on the same SPY/VIX series.
	currentVix := vixCloses[len(vixCloses)-1]
	vixSMA20 := mean(vixCloses[len(vixCloses)-20:])
	vixP80 := percentileValue(vixCloses, 80)
	spyNow := spyCloses[len(spyCloses)-1]
	spySMA50 := mean(spyCloses[len(spyCloses)-50:])
	spySMA200 := mean(spyCloses[len(spyCloses)-200:])
	spy5d := returnNAgo(spyCloses, 4) // see classifyRegime: QC's spy_5d_ret is 4-bar
	batch.Annotate("vix", fmt.Sprintf("%.4f", currentVix))
	batch.Annotate("vix_sma20", fmt.Sprintf("%.4f", vixSMA20))
	batch.Annotate("vix_p80", fmt.Sprintf("%.4f", vixP80))
	batch.Annotate("spy", fmt.Sprintf("%.4f", spyNow))
	batch.Annotate("spy_sma50", fmt.Sprintf("%.4f", spySMA50))
	batch.Annotate("spy_sma200", fmt.Sprintf("%.4f", spySMA200))
	batch.Annotate("spy_5d_ret", fmt.Sprintf("%.6f", spy5d))

	// Distribute the equity sleeve across the top-4 with the ML overweight
	// tilt and per-name caps applied.
	equityTotal := s.LongGross * decision.equityWeight
	weights := s.allocateTopWeights(equityTotal, decision.mlBullish)

	// Apply trailing-stop adjustments: stage 0 holds full weight, stage 1
	// trims to 2/3, stage 2 to 1/3, stage 3 liquidates. Stage transitions
	// happen inside maintainLongTrails.
	s.maintainLongTrails(port, today)

	// Names dropped from the top-4 since the previous month are exited
	// implicitly: weightPlan never adds them, RebalanceTo liquidates anything
	// not in the new allocation. Their trail state is cleaned up here.
	current := assetSet(s.topSet)
	for a := range s.longTrail {
		if _, kept := current[a]; !kept {
			delete(s.longTrail, a)
		}
	}

	// QC's SetHoldings has a tolerance (default 0.0025 = 0.25% of portfolio
	// value) that suppresses orders when the existing position is already
	// within the tolerance band of the target. Pvbt's RebalanceTo has no
	// such tolerance and fires an adjustment whenever the realized weight
	// differs from the target by any amount, which produced 3-6x more
	// long-sleeve orders than QC and added meaningful slippage drag.
	// Replicate QC's tolerance: if realized weight is within 0.25% of
	// target, emit realized (no order fires); else emit target.
	const longTolerance = 0.0025

	portValue := port.Value()
	for i, sym := range s.topSet {
		if i >= len(weights) {
			break
		}
		base := weights[i]
		target := s.applyLongTrailStage(sym, base, port)
		emit := target
		if portValue > 0 && target > 0 {
			realized := port.PositionValue(sym) / portValue
			if math.Abs(target-realized) < longTolerance {
				emit = realized
			}
		}
		plan.add(sym, emit)
		s.ensureLongTrailState(sym, base, port)
	}

	gldTarget := s.LongGross * decision.goldWeight
	gldEmit := gldTarget
	if portValue > 0 && gldTarget > 0 {
		gldRealized := port.PositionValue(s.gld) / portValue
		if math.Abs(gldTarget-gldRealized) < longTolerance {
			gldEmit = gldRealized
		}
	}
	plan.add(s.gld, gldEmit)
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
	// 4-bar form here to match QC's panic-branch threshold exactly. Calling
	// returnNAgo(closes, 5) would compute the natural 5-bar return and shift
	// the panic boundary by enough to flip the regime decision on a handful
	// of borderline days each year.
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

// maintainLongTrails walks each tracked long position, advancing the high-
// water mark and the trailing-stop stage. Liquidations (stage 3) drop the
// entry from the trail map; the absence of a weight in the plan triggers the
// actual sell via RebalanceTo.
func (s *LongShortHarvest) maintainLongTrails(port portfolio.Portfolio, today time.Time) {
	for sym, st := range s.longTrail {
		qty := port.Position(sym)
		if qty <= 0 {
			delete(s.longTrail, sym)
			continue
		}

		px := s.lastKnownPrice(port, sym)
		if math.IsNaN(px) || px <= 0 {
			continue
		}

		if px > st.high {
			st.high = px
		}
		dd := 0.0
		if st.high > 0 {
			dd = (st.high - px) / st.high
		}

		switch st.stage {
		case 0:
			if dd >= s.LongTrail1 {
				st.stage = 1
				st.high = px
			}
		case 1:
			if dd >= s.LongTrail2 {
				st.stage = 2
				st.high = px
			}
		case 2:
			if dd >= s.LongTrail3 {
				st.stage = 3
			}
		}
	}
	_ = today
}

// applyLongTrailStage scales a base target weight by the trim factor implied
// by the current trailing-stop stage for that asset.
func (s *LongShortHarvest) applyLongTrailStage(sym asset.Asset, base float64, port portfolio.Portfolio) float64 {
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

// ensureLongTrailState creates the per-position trail entry on first
// inclusion or refreshes the recorded target weight when sizing changes. The
// high-water mark is initialized to the current price so the first stop
// trigger requires a real drawdown rather than an immediate trim.
func (s *LongShortHarvest) ensureLongTrailState(sym asset.Asset, target float64, port portfolio.Portfolio) {
	if target <= 0 {
		delete(s.longTrail, sym)
		return
	}

	px := s.lastKnownPrice(port, sym)
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

// lastKnownPrice returns the most recent close for the asset from the
// portfolio's price snapshot. Prefer this over the engine's data store
// because it reflects the prices the engine just updated for this step.
func (s *LongShortHarvest) lastKnownPrice(port portfolio.Portfolio, sym asset.Asset) float64 {
	prices := port.Prices()
	if prices == nil {
		return math.NaN()
	}
	col := prices.Column(sym, data.MetricClose)
	if len(col) == 0 {
		return math.NaN()
	}
	for i := len(col) - 1; i >= 0; i-- {
		if !math.IsNaN(col[i]) {
			return col[i]
		}
	}
	return math.NaN()
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
