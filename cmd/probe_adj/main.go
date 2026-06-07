// Copyright 2026
// SPDX-License-Identifier: Apache-2.0
//
// Diagnostic-only: derives pvbt's SPY adjustment factor (AdjClose/Close) for
// each trading day in 2015 and prints alongside dividend + split metadata so
// we can see whether the factor changes only on ex-div dates (pure dividend
// adjustment), only on split dates (split adjustment), or both.

package main

import (
	"context"
	"fmt"
	"math"
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
	spy, err := provider.LookupAsset(ctx, "SPY")
	if err != nil {
		panic(err)
	}

	start := time.Date(2014, 12, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2016, 1, 31, 0, 0, 0, 0, time.UTC)

	df, err := provider.Fetch(ctx, data.DataRequest{
		Assets: []asset.Asset{spy},
		Metrics: []data.Metric{
			data.MetricOpen, data.MetricHigh, data.MetricLow, data.MetricClose,
			data.AdjClose, data.Dividend, data.SplitFactor,
		},
		Start: start,
		End:   end,
	})
	if err != nil {
		panic(err)
	}

	ts := df.Times()
	op := df.Column(spy, data.MetricOpen)
	hi := df.Column(spy, data.MetricHigh)
	lo := df.Column(spy, data.MetricLow)
	cl := df.Column(spy, data.MetricClose)
	ac := df.Column(spy, data.AdjClose)
	dv := df.Column(spy, data.Dividend)
	sf := df.Column(spy, data.SplitFactor)

	// Print header
	fmt.Println("date,open,high,low,close,adj_close,factor,div,split,factor_diff_pct")

	prevFactor := math.NaN()
	for i := range ts {
		factor := math.NaN()
		if cl[i] > 0 {
			factor = ac[i] / cl[i]
		}
		dvi := dv[i]
		if math.IsNaN(dvi) {
			dvi = 0
		}
		sfi := sf[i]
		if math.IsNaN(sfi) {
			sfi = 1
		}
		factorDiffPct := math.NaN()
		if !math.IsNaN(prevFactor) && prevFactor > 0 {
			factorDiffPct = (factor/prevFactor - 1) * 100
		}
		fmt.Printf("%s,%.4f,%.4f,%.4f,%.4f,%.4f,%.6f,%.4f,%.4f,%.4f\n",
			ts[i].Format("2006-01-02"),
			op[i], hi[i], lo[i], cl[i], ac[i], factor, dvi, sfi, factorDiffPct)
		prevFactor = factor
	}
}
