// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"math"
	"sort"
)

// atr computes the per-bar range proxy used by the QC reference: the average
// of max(hi-lo, |hi-cl|, |lo-cl|) over the trailing n bars, where hi/lo/cl
// are from the SAME bar (no previous-close gap adjustment). With normal OHLC
// where hi >= cl >= lo this collapses to the average daily range. Matches QC's
// `_atr` exactly so Hurst-like scores are comparable; stops use the same
// quantity so risk is sized off the same number.
func atr(highs, lows, closes []float64, n int) float64 {
	w := len(closes)
	if w < n || n < 1 {
		return math.NaN()
	}
	if len(highs) != w || len(lows) != w {
		return math.NaN()
	}
	sum := 0.0
	for i := w - n; i < w; i++ {
		hi := highs[i]
		lo := lows[i]
		cl := closes[i]
		if math.IsNaN(hi) || math.IsNaN(lo) || math.IsNaN(cl) {
			return math.NaN()
		}
		tr := hi - lo
		if v := math.Abs(hi - cl); v > tr {
			tr = v
		}
		if v := math.Abs(lo - cl); v > tr {
			tr = v
		}
		sum += tr
	}
	return sum / float64(n)
}

// hurstLike returns a Hurst-like persistence measure over the trailing n bars.
// It is the QC `_hurst_like` formula: log(range)/log(n)/log(atr) bumped toward
// 0.5 by `bump`. Returns NaN if inputs are insufficient.
func hurstLike(highs, lows, closes []float64, n int, bump float64) float64 {
	a := atr(highs, lows, closes, n)
	if math.IsNaN(a) || a <= 0 {
		return math.NaN()
	}
	w := len(closes)
	hi := math.Inf(-1)
	lo := math.Inf(1)
	for i := w - n; i < w; i++ {
		if highs[i] > hi {
			hi = highs[i]
		}
		if lows[i] < lo {
			lo = lows[i]
		}
	}
	span := hi - lo
	if span <= 0 || math.IsInf(span, 0) {
		return math.NaN()
	}
	h := (math.Log(span) - math.Log(a)) / math.Log(float64(n))
	switch {
	case h > 0.45:
		h += bump
	case h < 0.45:
		h -= bump
	}
	return h
}

// mean returns the arithmetic mean of values, ignoring NaNs. Returns NaN if
// no finite values are present.
func mean(values []float64) float64 {
	sum := 0.0
	count := 0
	for _, v := range values {
		if !math.IsNaN(v) {
			sum += v
			count++
		}
	}
	if count == 0 {
		return math.NaN()
	}
	return sum / float64(count)
}

// stdDev returns the sample standard deviation of values, ignoring NaNs.
// Returns NaN if fewer than two finite values are present.
func stdDev(values []float64) float64 {
	m := mean(values)
	if math.IsNaN(m) {
		return math.NaN()
	}
	count := 0
	ss := 0.0
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		d := v - m
		ss += d * d
		count++
	}
	if count < 2 {
		return math.NaN()
	}
	return math.Sqrt(ss / float64(count-1))
}

// percentileRank returns the fraction of values strictly less than the
// reference; matches the QC `np.sum(vix_closes < current_vix)/len(vix_closes)`
// shape. Returns NaN on empty input.
func percentileRank(values []float64, reference float64) float64 {
	if len(values) == 0 || math.IsNaN(reference) {
		return math.NaN()
	}
	count := 0
	below := 0
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		count++
		if v < reference {
			below++
		}
	}
	if count == 0 {
		return math.NaN()
	}
	return float64(below) / float64(count)
}

// percentileValue returns the requested percentile (0-100) of values using
// linear interpolation between the nearest ranks (numpy default).
func percentileValue(values []float64, p float64) float64 {
	clean := make([]float64, 0, len(values))
	for _, v := range values {
		if !math.IsNaN(v) {
			clean = append(clean, v)
		}
	}
	if len(clean) == 0 {
		return math.NaN()
	}
	sort.Float64s(clean)
	if p <= 0 {
		return clean[0]
	}
	if p >= 100 {
		return clean[len(clean)-1]
	}
	rank := p / 100.0 * float64(len(clean)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return clean[lo]
	}
	frac := rank - float64(lo)
	return clean[lo] + frac*(clean[hi]-clean[lo])
}

// pctReturns returns the day-over-day simple returns of closes. The first
// element is NaN.
func pctReturns(closes []float64) []float64 {
	out := make([]float64, len(closes))
	if len(closes) == 0 {
		return out
	}
	out[0] = math.NaN()
	for i := 1; i < len(closes); i++ {
		prev := closes[i-1]
		if math.IsNaN(prev) || prev == 0 {
			out[i] = math.NaN()
			continue
		}
		out[i] = (closes[i] - prev) / prev
	}
	return out
}
