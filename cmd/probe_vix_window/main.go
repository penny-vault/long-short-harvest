// Copyright 2026
// SPDX-License-Identifier: Apache-2.0
//
// Diagnostic-only: prints the VIX window as pvbt's regime classifier sees it
// on 2015-01-05, sorted high-to-low, plus the 80th percentile and a few
// distribution markers. Compare the values to QC's reference (vix_p80=18.68).

package main

import (
	"context"
	"fmt"
	"math"
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
	vix := asset.NewFREDAsset("VIXCLS")

	end := time.Date(2015, 1, 5, 16, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -200) // wide pull, slice to last 100

	df, err := provider.Fetch(ctx, data.DataRequest{
		Assets:  []asset.Asset{vix},
		Metrics: []data.Metric{data.MetricClose},
		Start:   start,
		End:     end,
	})
	if err != nil {
		panic(err)
	}

	times := df.Times()
	closes := df.Column(vix, data.MetricClose)
	clean := make([]struct {
		t time.Time
		v float64
	}, 0, len(times))
	for i, t := range times {
		if !math.IsNaN(closes[i]) {
			clean = append(clean, struct {
				t time.Time
				v float64
			}{t, closes[i]})
		}
	}

	if len(clean) < 100 {
		fmt.Printf("only %d bars available, expected >=100\n", len(clean))
		return
	}
	tail := clean[len(clean)-100:]
	fmt.Printf("Window: %s to %s, %d bars\n",
		tail[0].t.Format("2006-01-02"),
		tail[len(tail)-1].t.Format("2006-01-02"),
		len(tail))

	vals := make([]float64, len(tail))
	for i, b := range tail {
		vals[i] = b.v
	}
	sort.Float64s(vals)

	// numpy linear-interp percentile
	pct := func(p float64) float64 {
		rank := p / 100.0 * float64(len(vals)-1)
		lo := int(math.Floor(rank))
		hi := int(math.Ceil(rank))
		if lo == hi {
			return vals[lo]
		}
		frac := rank - float64(lo)
		return vals[lo] + frac*(vals[hi]-vals[lo])
	}

	fmt.Printf("min=%.4f max=%.4f mean=%.4f\n",
		vals[0], vals[len(vals)-1], avg(vals))
	fmt.Printf("p20=%.4f p50=%.4f p80=%.4f p90=%.4f p95=%.4f\n",
		pct(20), pct(50), pct(80), pct(90), pct(95))

	// Top 25 highest values with dates
	type tv struct {
		t time.Time
		v float64
	}
	all := make([]tv, len(tail))
	for i, b := range tail {
		all[i] = tv{b.t, b.v}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	fmt.Println("\nTop 25 highest VIX in window:")
	for i := 0; i < 25; i++ {
		fmt.Printf("  %s  %.4f\n", all[i].t.Format("2006-01-02"), all[i].v)
	}
}

func avg(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
