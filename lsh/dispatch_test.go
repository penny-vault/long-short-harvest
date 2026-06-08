// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"math"
	"testing"
	"time"
)

// TestClassifySlot_MapsOffsetsToRoutines checks the four lsh.py post-open
// offsets map to their slots and every other minute is a no-op, across both
// EST and EDT so the conversion is DST-invariant.
func TestClassifySlot_MapsOffsetsToRoutines(t *testing.T) {
	mk := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, nyc)
	}

	cases := []struct {
		name string
		when time.Time
		want slot
	}{
		// Summer (EDT).
		{"edt 10:00 signal", mk(2015, 6, 1, 10, 0), slotSignalLong},
		{"edt 10:30 train", mk(2015, 6, 1, 10, 30), slotTrain},
		{"edt 11:00 risk-long", mk(2015, 6, 1, 11, 0), slotRiskLong},
		{"edt 12:10 risk-short", mk(2015, 6, 1, 12, 10), slotRiskShort},
		{"edt 10:10 none", mk(2015, 6, 1, 10, 10), slotNone},
		{"edt 09:30 open none", mk(2015, 6, 1, 9, 30), slotNone},
		{"edt 12:00 none", mk(2015, 6, 1, 12, 0), slotNone},
		// Winter (EST).
		{"est 10:00 signal", mk(2015, 1, 5, 10, 0), slotSignalLong},
		{"est 11:00 risk-long", mk(2015, 1, 5, 11, 0), slotRiskLong},
		{"est 12:10 risk-short", mk(2015, 1, 5, 12, 10), slotRiskShort},
		// Day after spring-forward and fall-back transitions.
		{"dst spring 10:00", mk(2015, 3, 9, 10, 0), slotSignalLong},
		{"dst fall 10:30", mk(2015, 11, 2, 10, 30), slotTrain},
	}

	for _, tc := range cases {
		if got := classifySlot(tc.when); got != tc.want {
			t.Errorf("%s: classifySlot(%s) = %d, want %d", tc.name, tc.when, got, tc.want)
		}
	}
}

// TestClassifySlot_ConvertsFromUTC confirms a UTC-typed firing time is
// converted to Eastern before classification: 14:00 UTC is 10:00 EDT.
func TestClassifySlot_ConvertsFromUTC(t *testing.T) {
	utc14 := time.Date(2015, 6, 1, 14, 0, 0, 0, time.UTC) // 10:00 EDT
	if got := classifySlot(utc14); got != slotSignalLong {
		t.Fatalf("14:00 UTC should map to slotSignalLong (10:00 EDT), got %d", got)
	}
	utc15 := time.Date(2015, 1, 5, 15, 0, 0, 0, time.UTC) // 10:00 EST
	if got := classifySlot(utc15); got != slotSignalLong {
		t.Fatalf("15:00 UTC should map to slotSignalLong (10:00 EST), got %d", got)
	}
}

// TestClampWeight_RespectsHeadroomAndBuffer covers QC's _safe_set_holdings
// clamp: the target passes through when headroom is ample, binds to the
// available margin otherwise, and always reserves the 2.5% free-value buffer.
func TestClampWeight_RespectsHeadroomAndBuffer(t *testing.T) {
	const pv = 100000.0

	// Ample headroom (3x cap, flat position): 0.3 passes unchanged.
	if got := clampWeight(0.3, pv, 0, 3*pv); math.Abs(got-0.3) > 1e-12 {
		t.Fatalf("ample headroom should not clamp; got %v", got)
	}

	// Long clamp: flat position, headroom = 1.0x => maxAbs = 1 - 0.025.
	if got := clampWeight(2.0, pv, 0, pv); math.Abs(got-0.975) > 1e-12 {
		t.Fatalf("long clamp expected 0.975, got %v", got)
	}

	// Short clamp is symmetric.
	if got := clampWeight(-2.0, pv, 0, pv); math.Abs(got+0.975) > 1e-12 {
		t.Fatalf("short clamp expected -0.975, got %v", got)
	}

	// Current position contributes to capacity: curW=0.4, no headroom =>
	// maxAbs = 0.4 - 0.025 = 0.375.
	if got := clampWeight(0.5, pv, 0.4*pv, 0); math.Abs(got-0.375) > 1e-12 {
		t.Fatalf("position-weight capacity expected 0.375, got %v", got)
	}

	// Buffer exactly consumes the headroom => no capacity for new exposure.
	if got := clampWeight(0.5, pv, 0, freePortfolioValuePct*pv); got != 0 {
		t.Fatalf("buffer should zero out capacity; got %v", got)
	}

	// Non-positive portfolio value yields zero (no order would be emitted).
	if got := clampWeight(0.3, 0, 0, pv); got != 0 {
		t.Fatalf("pv<=0 should clamp to 0, got %v", got)
	}
}
