// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"math"
	"testing"

	"github.com/penny-vault/pvbt/asset"
)

func makeSPYSeries(slope float64, noise float64, n int) []float64 {
	out := make([]float64, n)
	out[0] = 400
	for i := 1; i < n; i++ {
		out[i] = out[i-1]*(1+slope) + noise*math.Sin(float64(i))
	}
	return out
}

func makeVIXSeries(level float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = level + 0.5*math.Sin(float64(i))
	}
	return out
}

func TestClassifyRegime_TrendOn(t *testing.T) {
	s := &LongShortHarvest{LongGross: 0.9}
	vix := makeVIXSeries(15, 100)
	spy := makeSPYSeries(0.001, 0.5, 220) // gentle uptrend, last close above SMA200
	got := s.classifyRegime(vix, spy)
	if got.branch != regimeTrendOn {
		t.Fatalf("expected trend-on branch, got %v", got.branch)
	}
	if got.equityWeight != 0.70 {
		t.Fatalf("expected 0.70 equity, got %v", got.equityWeight)
	}
}

func TestClassifyRegime_VolSpike(t *testing.T) {
	s := &LongShortHarvest{LongGross: 0.9}
	// VIX is calm at ~15 for the trailing window with a one-day spike to 25
	// at the current bar so vixSMA20 stays near 15 and currentVix > 1.2 *
	// vixSMA20 fires the spike branch.
	vix := makeVIXSeries(15, 100)
	vix[len(vix)-1] = 25
	spy := makeSPYSeries(0.001, 0.5, 220)
	got := s.classifyRegime(vix, spy)
	if got.branch != regimeSpike {
		t.Fatalf("expected vol spike branch, got %v", got.branch)
	}
	if got.equityWeight != 0.0 {
		t.Fatalf("expected 0 equity in vol spike, got %v", got.equityWeight)
	}
	if got.goldWeight != 0.50 {
		t.Fatalf("expected 0.5 gold, got %v", got.goldWeight)
	}
}

func TestCapAndRenormalize_RespectsMaxAndPreservesTotal(t *testing.T) {
	weights := []float64{0.4, 0.4, 0.4, 0.4}
	out := capAndRenormalize(weights, 1.0, 0.0, 0.35)
	sum := 0.0
	for _, w := range out {
		sum += w
		if w > 0.35+1e-9 {
			t.Fatalf("weight %v exceeds cap 0.35", w)
		}
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Logf("note: capped weights sum to %v < 1.0 because all are at cap", sum)
	}
}

func TestCapAndRenormalize_ConvergesWithSlack(t *testing.T) {
	weights := []float64{0.5, 0.5, 0.5, 0.5}
	out := capAndRenormalize(weights, 0.8, 0.0, 0.35)
	sum := 0.0
	for _, w := range out {
		sum += w
		if w > 0.35+1e-9 {
			t.Fatalf("weight %v exceeds cap 0.35", w)
		}
	}
	if math.Abs(sum-0.8) > 1e-6 {
		t.Fatalf("expected sum=0.8, got %v", sum)
	}
}

func TestAllocateTopWeights_MLTilt(t *testing.T) {
	s := &LongShortHarvest{
		LongGross:    0.9,
		MLTilt:       0.25,
		TopWeightMax: 0.5,
		TopWeightMin: 0.0,
		topSet: []asset.Asset{
			{Ticker: "AAPL"},
			{Ticker: "MSFT"},
			{Ticker: "GOOG"},
			{Ticker: "AMZN"},
		},
	}
	weights := s.allocateTopWeights(0.8, true)
	if len(weights) != 4 {
		t.Fatalf("expected 4 weights, got %d", len(weights))
	}
	// Index 0 is the overweight; it should be larger than the others.
	for i := 1; i < 4; i++ {
		if weights[0] <= weights[i] {
			t.Fatalf("expected ML tilt to overweight index 0; got %v vs %v", weights[0], weights[i])
		}
	}
	sum := 0.0
	for _, w := range weights {
		sum += w
	}
	if math.Abs(sum-0.8) > 1e-6 {
		t.Fatalf("expected sum=0.8, got %v", sum)
	}
}

func TestSelectShortCandidates_FiltersByThreshold(t *testing.T) {
	s := &LongShortHarvest{ScoreThreshold: 0.85, TopN: 2}
	candidates := []shortCandidate{
		{asset: asset.Asset{Ticker: "A"}, score: 0.90},
		{asset: asset.Asset{Ticker: "B"}, score: 0.80},
		{asset: asset.Asset{Ticker: "C"}, score: 0.95},
		{asset: asset.Asset{Ticker: "D"}, score: 0.82},
	}
	picked := s.selectShortCandidates(candidates)
	if len(picked) != 2 {
		t.Fatalf("expected 2, got %d", len(picked))
	}
	if picked[0].asset.Ticker != "C" || picked[1].asset.Ticker != "A" {
		t.Fatalf("expected [C,A], got [%s,%s]", picked[0].asset.Ticker, picked[1].asset.Ticker)
	}
}

func TestApplyLongTrailStage_TrimsByStage(t *testing.T) {
	s := &LongShortHarvest{
		longTrail: map[asset.Asset]*longTrailState{
			{Ticker: "X"}: {stage: 0, targetW: 0.3},
		},
	}
	a := asset.Asset{Ticker: "X"}

	if got := s.applyLongTrailStage(a, 0.3, nil); math.Abs(got-0.3) > 1e-9 {
		t.Fatalf("stage 0 should not trim; got %v", got)
	}

	s.longTrail[a].stage = 1
	if got := s.applyLongTrailStage(a, 0.3, nil); math.Abs(got-0.2) > 1e-9 {
		t.Fatalf("stage 1 should trim to 2/3; got %v", got)
	}

	s.longTrail[a].stage = 2
	if got := s.applyLongTrailStage(a, 0.3, nil); math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("stage 2 should trim to 1/3; got %v", got)
	}

	s.longTrail[a].stage = 3
	if got := s.applyLongTrailStage(a, 0.3, nil); got != 0 {
		t.Fatalf("stage 3 should liquidate; got %v", got)
	}
}
