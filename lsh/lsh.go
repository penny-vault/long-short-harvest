// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"time"

	"github.com/penny-vault/pvbt/asset"
	"github.com/penny-vault/pvbt/data"
	"github.com/penny-vault/pvbt/engine"
	"github.com/penny-vault/pvbt/portfolio"
	"github.com/penny-vault/pvbt/universe"
	"github.com/rs/zerolog"
)

//go:embed README.md
var description string

// LongShortHarvest is a long-short equity strategy combining a regime-driven
// concentrated long sleeve in the largest U.S. equities with a trend-
// exhaustion short sleeve drawn from the broader liquid US universe.
type LongShortHarvest struct {
	LongGross  float64 `pvbt:"long-gross" desc:"Maximum gross exposure of the long sleeve" default:"0.9"`
	ShortGross float64 `pvbt:"short-gross" desc:"Maximum gross exposure (absolute value) of the short sleeve" default:"0.6"`

	MLTilt       float64 `pvbt:"ml-tilt" desc:"Fraction of base weight added to the largest top-4 name when the random-forest probability is bullish" default:"0.25"`
	TopWeightMax float64 `pvbt:"top-weight-max" desc:"Maximum per-name weight in the long sleeve" default:"0.35"`
	TopWeightMin float64 `pvbt:"top-weight-min" desc:"Minimum per-name weight in the long sleeve" default:"0.0"`

	MaxUniverse int `pvbt:"max-universe" desc:"Maximum number of liquid US equities considered for the short sleeve. Default 300 because pvbt's USTradable includes ~1800 names with a heavy mega-cap dollar-volume tail; QC's f.DollarVolume rankings reach mid-cap candidates like ESPR/KLAC at top-150 but pvbt needs a wider net to capture the same names." default:"300"`
	TopN        int `pvbt:"top-n" desc:"Number of short candidates held at any time" default:"1"`

	MinHistoryDays int `pvbt:"min-history-days" desc:"Minimum trading days of contiguous price history required to be eligible (one trading year)" default:"252"`

	LookbackBars int `pvbt:"lookback-bars" desc:"Per-name daily-bar lookback used by the short sleeve Hurst-like score" default:"260"`
	SMALen       int `pvbt:"sma-len" desc:"Short-sleeve trend filter SMA length" default:"195"`

	ExtK           float64 `pvbt:"ext-k" desc:"Extension multiplier on ATR(20) gating short entries" default:"2.0"`
	MomK           float64 `pvbt:"mom-k" desc:"Momentum multiplier on ATR(20) gating short entries" default:"1.75"`
	ScoreThreshold float64 `pvbt:"score-threshold" desc:"Hurst-like composite score threshold for short candidates" default:"0.85"`
	StopATR        float64 `pvbt:"stop-atr" desc:"Adverse-move multiple of entry ATR that liquidates a short" default:"2.0"`

	LongTrail1 float64 `pvbt:"long-trail-1" desc:"Stage-1 trailing-stop drawdown for long names (trim to two-thirds)" default:"0.095"`
	LongTrail2 float64 `pvbt:"long-trail-2" desc:"Stage-2 trailing-stop drawdown for long names (trim to one-third)" default:"0.07"`
	LongTrail3 float64 `pvbt:"long-trail-3" desc:"Stage-3 trailing-stop drawdown for long names (liquidate)" default:"0.0485"`

	// --- internal state populated in Setup, mutated during Compute ---

	spy asset.Asset
	gld asset.Asset
	vix asset.Asset

	// Sibling-class duplicates of consolidated-mcap tickers in pvbt's
	// data. Sharadar puts the consolidated Berkshire mcap on BRK/B (with
	// BRK/A's mcap NaN) and the consolidated Alphabet mcap on GOOGL (with
	// GOOG's mcap NaN). QC's Morningstar data exposes both classes with
	// the same mcap, which lets BRK/A and GOOG enter QC's top-4 and
	// produces effective behavior pvbt can only replicate by inserting
	// the missing-mcap sibling alongside its consolidated twin. See
	// refreshLongUniverse for the rule.
	brkA asset.Asset
	goog asset.Asset

	usTradable universe.Universe

	topSet           []asset.Asset
	topSetMonth      time.Month
	topSetYear       int
	activeShorts     []asset.Asset
	activeShortMonth time.Month
	activeShortYear  int

	model           *regimeModel
	lastRetrainKey  int
	lastUniverseKey int

	longTrail  map[asset.Asset]*longTrailState
	shortEntry map[asset.Asset]*shortEntryState
}

// longTrailState tracks per-position high-water mark and trailing-stop stage
// for the long sleeve. The full target weight is recorded so each stage can
// compute its trim level off the pre-stop sizing.
type longTrailState struct {
	high    float64
	stage   int
	targetW float64
}

// shortEntryState records the entry price and ATR(20) captured when a short
// position was opened. Both feed the ATR-distance stop in riskCheckShort,
// mirroring QC's self._entry dict.
type shortEntryState struct {
	entryPrice float64
	entryATR   float64
}

// Name returns the human-readable strategy name.
func (s *LongShortHarvest) Name() string { return "Long Short Harvest" }

// Setup resolves fixed assets and the US-tradable universe.
func (s *LongShortHarvest) Setup(eng *engine.Engine) {
	s.spy = eng.Asset("SPY")
	s.gld = eng.Asset("GLD")
	s.vix = asset.NewFREDAsset("VIXCLS")
	s.brkA = eng.Asset("BRK/A")
	s.goog = eng.Asset("GOOG")
	s.usTradable = eng.IndexUniverse("us-tradable")
	s.longTrail = make(map[asset.Asset]*longTrailState)
	s.shortEntry = make(map[asset.Asset]*shortEntryState)
	s.lastRetrainKey = -1
}

// Describe returns the strategy metadata. The schedule fires on the NYC
// regular-trading-hours 10-minute grid; Compute dispatches each firing to the
// lsh.py routine whose +30/+60/+90/+160-minute-after-open slot it matches
// (10:00/10:30/11:00/12:10 ET) and no-ops otherwise. Benchmarks SPY and warms
// up enough history for the random-forest retrain (800 trading days).
func (s *LongShortHarvest) Describe() engine.StrategyDescription {
	return engine.StrategyDescription{
		ShortCode:   "lsh",
		Description: description,
		Source:      "https://github.com/penny-vault/strategies/tree/main/long-short-harvest",
		Version:     "0.1.0",
		VersionDate: time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC),
		Schedule:    "*/10 * * * 1-5",
		Benchmark:   "SPY",
		Warmup:      800,
		// LSH targets ~1.5x gross (long 0.9 + short 0.6) at entry. We need
		// drift headroom on top: a short that rallies adversely can grow
		// 50%+ in market value while the long sleeve stays at 0.9, pushing
		// realized gross above 2.0 even when the new-order desired entry
		// is well within design. Pvbt's MaxLeverage is an entry-time gate
		// that rejects orders pushing gross over the cap, so we set the
		// cap to 3.0 to mirror QC's effective "Reg-T initial margin only"
		// behavior (QC has no gross-leverage entry gate, only per-short
		// initial margin which equates to 1/0.5 = 2x per-short cap).
		MaxLeverage: 3.0,
	}
}

// Compute is the strategy entry point. The engine fires it on the NYC RTH
// 10-minute grid; classifySlot maps the firing time to the lsh.py routine
// whose post-open offset it matches and Compute runs only that routine. Each
// routine emits incremental Allocate/Liquidate orders (QC's
// SetHoldings/Liquidate analogues); held positions persist untouched between
// firings, so non-routine firings are no-ops. Order fills land at the next
// 1-minute bar after eng.Now(), and the risk routines read the live intraday
// price -- the two places lsh.py is intraday by design.
func (s *LongShortHarvest) Compute(ctx context.Context, eng *engine.Engine, port portfolio.Portfolio, batch *portfolio.Batch) error {
	log := zerolog.Ctx(ctx)
	now := eng.Now()
	today := eng.CurrentDate()

	switch classifySlot(now) {
	case slotSignalLong:
		// +30 min. Bootstrap the long top_set on Day 1 (QC's FineSelection
		// runs pre-open). The short pool is refreshed only on Mondays below.
		if err := s.maybeBootstrapLongUniverse(ctx, eng, today); err != nil {
			log.Error().Err(err).Msg("bootstrap long universe failed")
			return fmt.Errorf("bootstrap long universe: %w", err)
		}

		// CheckSignal_Long: regime allocation of the top-4 plus GLD.
		if err := s.checkSignalLong(ctx, eng, port, batch); err != nil {
			log.Error().Err(err).Msg("check signal long failed")
			return fmt.Errorf("check signal long: %w", err)
		}

		// Rebalance_Short runs Mondays, AFTER CheckSignal_Long (QC schedule
		// registration order). Refresh the short pool immediately before it --
		// QC recomputes self._active daily but only this Monday routine reads
		// it, so a Monday-only refresh is equivalent and avoids the costly
		// daily universe scan.
		if now.In(nyc).Weekday() == time.Monday {
			if err := s.refreshShortUniverse(ctx, eng, today); err != nil {
				log.Error().Err(err).Msg("refresh short universe failed")
				return fmt.Errorf("refresh short universe: %w", err)
			}
			if err := s.rebalanceShort(ctx, eng, port, batch); err != nil {
				log.Error().Err(err).Msg("rebalance short failed")
				return fmt.Errorf("rebalance short: %w", err)
			}
		}

		// Rotate the long top_set AFTER today's allocation used it; the new
		// set takes effect tomorrow, matching QC's FineSelection-then-next-
		// day-trade pattern. Names dropped from the set are exited by the next
		// day's checkSignalLong (LiquidateNonTopLongsOnly).
		if err := s.maybeRefreshLongUniverse(ctx, eng, today); err != nil {
			log.Error().Err(err).Msg("refresh long universe failed")
			return fmt.Errorf("refresh long universe: %w", err)
		}

	case slotTrain:
		// +60 min, month start. QC's TrainModel fires after CheckSignal_Long
		// has already used the prior model; the new one takes effect tomorrow.
		s.maybeRetrainModel(ctx, eng, today)

	case slotRiskLong:
		// +90 min. RiskCheck_Long advances the trailing-stop state machine off
		// the live intraday price and trims/liquidates on stage transitions.
		if err := s.riskCheckLong(ctx, eng, port, batch); err != nil {
			log.Error().Err(err).Msg("risk check long failed")
			return fmt.Errorf("risk check long: %w", err)
		}

	case slotRiskShort:
		// +160 min. RiskCheck_Short covers shorts whose live intraday price has
		// run StopATR * entryATR against the entry.
		if err := s.riskCheckShort(ctx, eng, port, batch); err != nil {
			log.Error().Err(err).Msg("risk check short failed")
			return fmt.Errorf("risk check short: %w", err)
		}

	default:
		// slotNone: not a routine time. Held positions persist; emit nothing.
	}

	return nil
}

// slot identifies which lsh.py intraday routine a firing maps to. lsh.py
// schedules each routine a fixed number of minutes after the 09:30 ET open,
// which for US equities is always a fixed wall-clock time.
type slot int

const (
	slotNone       slot = iota
	slotSignalLong      // 10:00 ET (+30): CheckSignal_Long (+ Rebalance_Short on Mondays)
	slotTrain           // 10:30 ET (+60): TrainModel (month start)
	slotRiskLong        // 11:00 ET (+90): RiskCheck_Long
	slotRiskShort       // 12:10 ET (+160): RiskCheck_Short
)

// nyc is the trading calendar's timezone. The engine guarantees this loads
// (it panics in its own init otherwise), so a load failure here is fatal.
var nyc = mustLoadNYC()

func mustLoadNYC() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic("lsh: load America/New_York: " + err.Error())
	}
	return loc
}

// classifySlot maps an Eastern firing time to its routine. Firings at any
// other minute (the ~38 other 10-minute-grid ticks per session) return
// slotNone and do nothing.
func classifySlot(now time.Time) slot {
	t := now.In(nyc)
	switch t.Hour()*60 + t.Minute() {
	case 10 * 60: // 10:00, +30 after the 09:30 open
		return slotSignalLong
	case 10*60 + 30: // 10:30, +60
		return slotTrain
	case 11 * 60: // 11:00, +90
		return slotRiskLong
	case 12*60 + 10: // 12:10, +160
		return slotRiskShort
	default:
		return slotNone
	}
}

// safeAllocate mirrors QC's _safe_set_holdings: it clamps the target weight to
// the live margin headroom before emitting the order so the broker's gross-
// leverage gate never rejects it (QC clamps to fit rather than reject). With
// pvbt's intraday marks, port.Value()/LeverageHeadroom() reflect the eng.Now()
// price. QC's max_abs = MarginRemaining / TotalPortfolioValue; the pvbt
// analogue is the name's current weight plus the remaining gross-notional
// headroom to the leverage cap, as a fraction of portfolio value, net of the
// 2.5% FreePortfolioValuePercentage reserve (lsh.py:17). In normal operation
// (gross ~1.5x under the 3x cap) the clamp never binds and w passes through.
func (s *LongShortHarvest) safeAllocate(ctx context.Context, port portfolio.Portfolio, batch *portfolio.Batch, sym asset.Asset, w float64) error {
	pv := port.Value()
	if pv <= 0 || math.IsNaN(pv) {
		return nil
	}
	// QC's SetHoldings sizes against (1 - FreePortfolioValuePercentage) of the
	// portfolio value -- lsh.py:17 reserves 2.5% as un-deployable buying power,
	// so the effective target is w*0.975*pv. Apply that reserve so our
	// deployment matches QC's 97.5% sizing instead of the full 100%.
	w *= 1 - freePortfolioValuePct
	w = clampWeight(w, pv, port.PositionValue(sym), port.LeverageHeadroom())

	// QC's SetHoldings skips an order when the adjustment is below LEAN's
	// minimum-order threshold (~0.25% of portfolio value). Without this, the
	// daily regime allocation emits a churn of tiny rebalancing fills on
	// ordinary price drift that QC suppresses -- inflating turnover and fill
	// count well above the reference. Skip when the dollar delta to the target
	// is within the tolerance band.
	if math.Abs(w*pv-port.PositionValue(sym)) < setHoldingsTolerance*pv {
		return nil
	}
	return batch.Allocate(ctx, sym, w)
}

// setHoldingsTolerance mirrors LEAN's minimum-order-margin threshold: orders
// whose adjustment is below this fraction of portfolio value are skipped by
// QC's SetHoldings.
const setHoldingsTolerance = 0.0025

// clampWeight applies QC's _safe_set_holdings clamp: limit |w| to the live
// margin headroom -- the name's current weight plus the remaining gross-notional
// headroom to the leverage cap, as fractions of portfolio value -- net of the
// FreePortfolioValuePercentage reserve. Returns w unchanged when the clamp does
// not bind (the normal case at ~1.5x gross under the 3x cap).
func clampWeight(w, pv, posValue, headroom float64) float64 {
	if pv <= 0 || math.IsNaN(pv) {
		return 0
	}
	curW := posValue / pv
	headroomW := headroom / pv
	maxAbs := math.Abs(curW) + headroomW - freePortfolioValuePct
	if maxAbs < 0 {
		maxAbs = 0
	}
	if w > maxAbs {
		return maxAbs
	}
	if w < -maxAbs {
		return -maxAbs
	}
	return w
}

// freePortfolioValuePercentage reserved by QC's
// Settings.FreePortfolioValuePercentage = 0.025 (lsh.py:17).
const freePortfolioValuePct = 0.025

// intradayPrice returns the live intraday close for sym at the current firing
// time -- the last 1-minute bar ending at eng.Now(). This is the pvbt analogue
// of QC's self.Securities[sym].Price, used ONLY by the three risk-sensitive
// reads (long-trail high-water init/drawdown, short ATR stop). Daily-history
// reads (regime, scoring, training) stay on the daily Fetch path. Returns NaN
// when no bar is available (treated by callers as "skip this name", matching
// lsh.py's `if px <= 0: continue`).
func (s *LongShortHarvest) intradayPrice(ctx context.Context, eng *engine.Engine, sym asset.Asset) float64 {
	m := s.intradayPrices(ctx, eng, []asset.Asset{sym})
	return m[sym]
}

// intradayPrices batch-fetches the live intraday close for each asset in one
// MinuteBars call, returning the last non-NaN close per asset (NaN if none).
// MinuteBars(5) tolerates a missing boundary minute.
func (s *LongShortHarvest) intradayPrices(ctx context.Context, eng *engine.Engine, syms []asset.Asset) map[asset.Asset]float64 {
	out := make(map[asset.Asset]float64, len(syms))
	if len(syms) == 0 {
		return out
	}
	df, err := eng.Fetch(ctx, syms, portfolio.MinuteBars(5), []data.Metric{data.MetricClose})
	if err != nil || df == nil {
		for _, sym := range syms {
			out[sym] = math.NaN()
		}
		return out
	}
	for _, sym := range syms {
		px := math.NaN()
		col := df.Column(sym, data.MetricClose)
		for i := len(col) - 1; i >= 0; i-- {
			if !math.IsNaN(col[i]) {
				px = col[i]
				break
			}
		}
		out[sym] = px
	}
	return out
}
