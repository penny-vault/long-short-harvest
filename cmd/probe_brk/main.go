// Copyright 2026
// SPDX-License-Identifier: Apache-2.0
//
// Diagnostic-only: prints pvbt's mcap for BRK/A and BRK/B on a 2015 date so we
// can compare against QC's (Morningstar) values. Hypothesis: Sharadar/pvbt
// report the consolidated Berkshire mcap on BRK/B (instead of just the B-class
// share count), while Morningstar splits A and B by share class.

package main

import (
	"context"
	"fmt"
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

	for _, ticker := range []string{"BRK/A", "BRK/B", "BRK.A", "BRK.B", "BRKA", "BRKB"} {
		la, le := provider.LookupAsset(ctx, ticker)
		fmt.Printf("=== %s ===\n", ticker)
		if le != nil {
			fmt.Printf("  LookupAsset err=%v\n\n", le)
			continue
		}
		fmt.Printf("  ticker=%s figi=%s name=%q\n", la.Ticker, la.CompositeFigi, la.Name)

		d := time.Date(2015, 4, 1, 16, 0, 0, 0, time.UTC)
		df, ferr := provider.Fetch(ctx, data.DataRequest{
			Assets:  []asset.Asset{la},
			Metrics: []data.Metric{data.MarketCap, data.MetricClose, data.Volume},
			Start:   d.AddDate(0, 0, -2),
			End:     d,
		})
		if ferr != nil {
			fmt.Printf("  fetch err: %v\n\n", ferr)
			continue
		}
		ts := df.Times()
		mcaps := df.Column(la, data.MarketCap)
		closes := df.Column(la, data.MetricClose)
		vols := df.Column(la, data.Volume)
		for i := range ts {
			fmt.Printf("  %s mcap=$%.2fB close=%.2f vol=%.0f\n",
				ts[i].Format("2006-01-02"), mcaps[i]/1e9, closes[i], vols[i])
		}
		fmt.Println()
	}
}
