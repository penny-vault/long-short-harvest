// Copyright 2026
// SPDX-License-Identifier: Apache-2.0
//
// Diagnostic-only: walks each 2015 trading day, computing QC's regime
// classifier from the SAME SPY+VIX closes pvbt sees, and outputs per-day:
//   date, regime_qc, vix_now, vix_p80, vix_sma20, spy_now, spy_5d, spy_sma200
//
// Use the resulting CSV to diff against the regime annotations recorded by
// the strategy backtest. If they agree per-day, the regime classifier is in
// sync with QC; if they don't, the divergence pinpoints where the rule
// evaluation differs (window slicing, percentile algorithm, etc.).

package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/penny-vault/pvbt/asset"
	"github.com/penny-vault/pvbt/data"
)

func main() {
	provider, err := data.NewPVDataProvider(nil)
	if err != nil {
		panic(err)
	}
	defer provider.Close()

	ctx := context.Background()

	spy := asset.Asset{Ticker: "SPY", CompositeFigi: "BBG000BDTBL9"}
	vix := asset.NewFREDAsset("VIXCLS")

	// Pull the full 2014-01-01 to 2016-01-01 window so we have 200 days of
	// SPY warm-up on Jan 2 2015.
	start := time.Date(2014, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)

	spyDF, err := provider.Fetch(ctx, data.DataRequest{
		Assets:  []asset.Asset{spy},
		Metrics: []data.Metric{data.MetricClose},
		Start:   start,
		End:     end,
	})
	if err != nil {
		panic(fmt.Sprintf("spy fetch: %v", err))
	}
	vixDF, err := provider.Fetch(ctx, data.DataRequest{
		Assets:  []asset.Asset{vix},
		Metrics: []data.Metric{data.MetricClose},
		Start:   start,
		End:     end,
	})
	if err != nil {
		panic(fmt.Sprintf("vix fetch: %v", err))
	}

	spyTimes := spyDF.Times()
	spyCloses := spyDF.Column(spy, data.MetricClose)
	vixTimes := vixDF.Times()
	vixCloses := vixDF.Column(vix, data.MetricClose)

	vixByDate := make(map[string]float64, len(vixTimes))
	for i, t := range vixTimes {
		vixByDate[t.Format("2006-01-02")] = vixCloses[i]
	}

	jan2 := time.Date(2015, 1, 2, 0, 0, 0, 0, time.UTC)

	fmt.Fprintln(os.Stdout, "date,regime,vix,vix_p80,vix_sma20,spy,spy_5d_ret,spy_sma50,spy_sma200")

	for i, t := range spyTimes {
		if t.Before(jan2) {
			continue
		}
		if t.Year() > 2015 {
			break
		}
		// Walk backward to gather the EXACT QC-shape windows: 100 VIX bars,
		// 200 SPY bars (each through and including today's close).
		spyEnd := i + 1
		spyStart := spyEnd - 200
		if spyStart < 0 {
			continue
		}
		spyWin := spyCloses[spyStart:spyEnd]

		// Build a parallel VIX window aligned to spy's last 100 trading days.
		// VIX skips weekends but otherwise tracks trading days; iterate
		// backward through SPY's calendar to find the matching 100 bars.
		vixWin := make([]float64, 0, 100)
		for j := spyEnd - 1; j >= 0 && len(vixWin) < 100; j-- {
			d := spyTimes[j].Format("2006-01-02")
			if v, ok := vixByDate[d]; ok && !math.IsNaN(v) {
				vixWin = append([]float64{v}, vixWin...)
			}
		}
		if len(vixWin) < 100 {
			continue
		}

		regime, intermediates := classifyQC(vixWin, spyWin)

		fmt.Fprintf(os.Stdout, "%s,%s,%.4f,%.4f,%.4f,%.4f,%.6f,%.4f,%.4f\n",
			t.Format("2006-01-02"),
			regime,
			intermediates.vixNow,
			intermediates.vixP80,
			intermediates.vixSMA20,
			intermediates.spyNow,
			intermediates.spy5dRet,
			intermediates.spySMA50,
			intermediates.spySMA200,
		)
	}
}

type regimeIntermediates struct {
	vixNow, vixP80, vixSMA20      float64
	spyNow, spy5dRet              float64
	spySMA50, spySMA200           float64
}

// classifyQC mirrors the exact branch order in lsh.py:360-433 with no ML
// (we record ml_bullish=False here since the random-forest rule is separate).
func classifyQC(vixCloses, spyCloses []float64) (string, regimeIntermediates) {
	r := regimeIntermediates{
		vixNow:    vixCloses[len(vixCloses)-1],
		vixSMA20:  mean(vixCloses[len(vixCloses)-20:]),
		vixP80:    percentile(vixCloses, 80),
		spyNow:    spyCloses[len(spyCloses)-1],
		spySMA50:  mean(spyCloses[len(spyCloses)-50:]),
		spySMA200: mean(spyCloses[len(spyCloses)-200:]),
		spy5dRet:  spyCloses[len(spyCloses)-1]/spyCloses[len(spyCloses)-5] - 1,
	}

	if r.vixNow > r.vixP80 && r.spy5dRet < -0.03 {
		return "panic", r
	}
	if r.vixNow < 13 && r.spyNow > r.spySMA50*1.05 {
		return "melt-up", r
	}
	if r.vixNow > 20 && r.vixNow < r.vixSMA20 {
		return "calm", r
	}
	if r.vixNow > r.vixSMA20*1.20 {
		return "spike", r
	}
	if r.spyNow > r.spySMA200 {
		return "trend-on", r
	}
	return "trend-off", r
}

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

// percentile mirrors numpy.percentile linear interpolation.
func percentile(values []float64, p float64) float64 {
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
