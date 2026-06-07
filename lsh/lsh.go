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

// shortEntryState records the entry price, ATR(20), and target weight at the
// moment a short position was opened. entryPrice and entryATR feed the
// ATR-distance stop; targetWeight is re-emitted daily so price drift between
// Monday rebalances doesn't push the realized gross over the leverage cap.
type shortEntryState struct {
	entryPrice  float64
	entryATR    float64
	targetWeight float64
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

// Describe returns the strategy metadata: it runs daily with internal date
// gating, benchmarks SPY, and warms up enough history for the random-forest
// retrain (800 trading days).
func (s *LongShortHarvest) Describe() engine.StrategyDescription {
	return engine.StrategyDescription{
		ShortCode:   "lsh",
		Description: description,
		Source:      "https://github.com/penny-vault/strategies/tree/main/long-short-harvest",
		Version:     "0.1.0",
		VersionDate: time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC),
		Schedule:    "@daily",
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

// Compute is the strategy entry point. It dispatches to the per-sleeve
// handlers based on internal date gates: the random-forest retrain and
// universe refresh fire on the first trading day of each month, the short
// rebalance on Mondays, and signal/risk checks every day.
func (s *LongShortHarvest) Compute(ctx context.Context, eng *engine.Engine, port portfolio.Portfolio, batch *portfolio.Batch) error {
	log := zerolog.Ctx(ctx)
	today := eng.CurrentDate()

	if err := s.maybeRefreshUniverses(ctx, eng, today); err != nil {
		log.Error().Err(err).Msg("universe refresh failed")
		return fmt.Errorf("refresh universes: %w", err)
	}

	weights := newWeightPlan()

	// Long sleeve: per-name target weights for the top-4 plus GLD, plus the
	// per-name trailing-stop adjustments.
	if err := s.computeLongSleeve(ctx, eng, port, batch, today, weights); err != nil {
		log.Error().Err(err).Msg("long sleeve failed")
		return fmt.Errorf("long sleeve: %w", err)
	}

	// Short sleeve: weekly Monday rebalance, daily ATR-stop check.
	if err := s.computeShortSleeve(ctx, eng, port, batch, today, weights); err != nil {
		log.Error().Err(err).Msg("short sleeve failed")
		return fmt.Errorf("short sleeve: %w", err)
	}

	members := weights.toMembers()

	// Cap-aware scaling: if the planned gross would exceed the account's
	// leverage cap, scale every weight proportionally so the request lands
	// safely under the cap. Without this the simulated broker rejects the
	// over-cap orders silently and the account drifts into a margin call,
	// which liquidates positions proportionally at adverse prices. We use a
	// 5% safety margin against the cap to absorb ordinary intraday drift.
	if cap := port.MaxLeverage(); cap > 0 {
		gross := 0.0
		for _, w := range members {
			gross += math.Abs(w)
		}
		safeCap := cap * 0.95
		if gross > safeCap {
			scale := safeCap / gross
			scaled := make(map[asset.Asset]float64, len(members))
			for a, w := range members {
				scaled[a] = w * scale
			}
			members = scaled
			weights.note(fmt.Sprintf("scaled by %.3f to fit cap %.2f", scale, cap))
		}
	}

	allocation := portfolio.Allocation{
		Date:          today,
		Members:       members,
		Justification: weights.justification(),
	}

	if err := batch.RebalanceTo(ctx, allocation); err != nil {
		log.Error().Err(err).Msg("rebalance failed")
		return fmt.Errorf("rebalance: %w", err)
	}

	// Refresh the long-sleeve top_set AFTER today's allocation has used the
	// existing top_set. The new top_set takes effect on the next trading
	// day, matching QC's FineSelection-then-next-day-trade pattern.
	if err := s.maybeRefreshLongUniverse(ctx, eng, today); err != nil {
		log.Error().Err(err).Msg("refresh long universe failed")
		return fmt.Errorf("refresh long universe: %w", err)
	}

	// Retrain the regime model AFTER the long sleeve consumed s.model. The
	// new model only takes effect on the next trading day, matching QC's
	// schedule (CheckSignal_Long at +30min, TrainModel at +60min).
	s.maybeRetrainModel(ctx, eng, today)

	return nil
}
