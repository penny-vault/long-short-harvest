// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"math"
	"testing"
)

func TestATR_KnownSeries(t *testing.T) {
	// Three days of OHLC where each bar's range is 2.0 (h=11, l=9, c=10).
	// QC's ATR averages max(h-l, |h-c|, |l-c|) per-bar; with c centered
	// between h and l this is just h-l = 2.0.
	highs := []float64{11, 11, 11}
	lows := []float64{9, 9, 9}
	closes := []float64{10, 10, 10}
	got := atr(highs, lows, closes, 2)
	if math.Abs(got-2.0) > 1e-9 {
		t.Fatalf("expected ATR=2.0, got %v", got)
	}
}

func TestATR_InsufficientHistory(t *testing.T) {
	highs := []float64{1, 2}
	lows := []float64{0, 1}
	closes := []float64{1, 2}
	if !math.IsNaN(atr(highs, lows, closes, 5)) {
		t.Fatal("expected NaN for insufficient history")
	}
}

func TestHurstLike_TrendingSeries(t *testing.T) {
	// A monotonically rising series should have a high Hurst-like value:
	// the price range over n bars is large relative to the per-bar ATR.
	n := 20
	highs := make([]float64, 30)
	lows := make([]float64, 30)
	closes := make([]float64, 30)
	for i := 0; i < 30; i++ {
		closes[i] = 100 + float64(i)
		highs[i] = closes[i] + 0.1
		lows[i] = closes[i] - 0.1
	}
	h := hurstLike(highs, lows, closes, n, 0.0)
	if math.IsNaN(h) {
		t.Fatal("expected finite Hurst value, got NaN")
	}
	if h <= 0.6 {
		t.Fatalf("expected Hurst > 0.6 for trending series, got %v", h)
	}
}

func TestPercentileRank_BasicShape(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	if got := percentileRank(values, 3); math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("expected 0.4, got %v", got)
	}
	if got := percentileRank(values, 0); got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
	if got := percentileRank(values, 6); got != 1 {
		t.Fatalf("expected 1, got %v", got)
	}
}

func TestPercentileValue_LinearInterp(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	if got := percentileValue(values, 50); math.Abs(got-3) > 1e-9 {
		t.Fatalf("expected 3, got %v", got)
	}
	if got := percentileValue(values, 0); got != 1 {
		t.Fatalf("expected 1, got %v", got)
	}
	if got := percentileValue(values, 100); got != 5 {
		t.Fatalf("expected 5, got %v", got)
	}
}

func TestStdDev_Sample(t *testing.T) {
	values := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	got := stdDev(values)
	want := 2.138089935
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestPctReturns(t *testing.T) {
	in := []float64{100, 110, 99}
	out := pctReturns(in)
	if !math.IsNaN(out[0]) {
		t.Fatal("first element must be NaN")
	}
	if math.Abs(out[1]-0.1) > 1e-9 {
		t.Fatalf("expected 0.1, got %v", out[1])
	}
	if math.Abs(out[2]-(-0.1)) > 1e-9 {
		t.Fatalf("expected -0.1, got %v", out[2])
	}
}
