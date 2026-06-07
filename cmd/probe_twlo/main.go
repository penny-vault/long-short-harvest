// Copyright 2026
// SPDX-License-Identifier: Apache-2.0
//
// Diagnostic-only: checks whether ESPR / SKX / KLAC are in pvbt's USTradable
// universe on the QC-reference short-rebalance dates and prints their mcap +
// trailing dvol.

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/penny-vault/pvbt/asset"
	"github.com/penny-vault/pvbt/data"
	"github.com/penny-vault/pvbt/universe"
)

func main() {
	provider, err := data.NewPVDataProvider(nil)
	if err != nil {
		panic(err)
	}
	defer provider.Close()

	ctx := context.Background()

	// Look up SKX via the asset provider directly, not just via the
	// us-tradable index. If LookupAsset returns a populated record, pvbt
	// knows about SKX in its asset master regardless of index membership.
	skxLookup, lookupErr := provider.LookupAsset(ctx, "SKX")
	fmt.Printf("=== SKX lookup direct ===\nerr=%v asset=%+v\n", lookupErr, skxLookup)

	checks := []struct {
		date    time.Time
		ticker  string
		figi    string
	}{
		{time.Date(2015, 3, 23, 16, 0, 0, 0, time.UTC), "ESPR", ""},
		{time.Date(2015, 8, 3, 16, 0, 0, 0, time.UTC), "SKX", ""},
		{time.Date(2015, 10, 26, 16, 0, 0, 0, time.UTC), "KLAC", ""},
	}

	for _, c := range checks {
		fmt.Printf("\n=== %s on %s ===\n", c.ticker, c.date.Format("2006-01-02"))
		u := universe.NewIndex(provider, "us-tradable")
		members := u.Assets(c.date)
		fmt.Printf("USTradable size: %d\n", len(members))

		var found *asset.Asset
		for i, m := range members {
			if m.Ticker == c.ticker {
				ms := m
				found = &ms
				_ = i
				break
			}
		}
		if found == nil {
			fmt.Printf("  %s NOT in USTradable on %s\n", c.ticker, c.date.Format("2006-01-02"))
			// Probe via direct lookup so we know whether pvbt has SKX at all.
			la, le := provider.LookupAsset(ctx, c.ticker)
			fmt.Printf("  LookupAsset err=%v -> %+v\n", le, la)
			if le == nil {
				// Try fetching mcap and price for SKX on the date even though
				// it's not in the index.
				df, ferr := provider.Fetch(ctx, data.DataRequest{
					Assets:  []asset.Asset{la},
					Metrics: []data.Metric{data.MarketCap, data.MetricClose, data.Volume},
					Start:   c.date.AddDate(0, 0, -3),
					End:     c.date,
				})
				if ferr != nil {
					fmt.Printf("  fetch err: %v\n", ferr)
				} else if df != nil {
					ts := df.Times()
					mcaps := df.Column(la, data.MarketCap)
					closes := df.Column(la, data.MetricClose)
					vols := df.Column(la, data.Volume)
					for i := range ts {
						fmt.Printf("    %s mcap=%v close=%v vol=%v\n",
							ts[i].Format("2006-01-02"), mcaps[i], closes[i], vols[i])
					}
				}
			}
			continue
		}
		fmt.Printf("  %s in USTradable: figi=%s listed=%s delisted=%s\n",
			c.ticker, found.CompositeFigi, found.Listed.Format("2006-01-02"),
			found.Delisted.Format("2006-01-02"))

		// Last day mcap + price/volume
		df, err := provider.Fetch(ctx, data.DataRequest{
			Assets:  []asset.Asset{*found},
			Metrics: []data.Metric{data.MarketCap, data.MetricClose, data.Volume},
			Start:   c.date.AddDate(0, 0, -10),
			End:     c.date,
		})
		if err != nil {
			fmt.Printf("  fetch error: %v\n", err)
			continue
		}
		if df == nil || df.Len() == 0 {
			fmt.Printf("  no rows returned\n")
			continue
		}
		ts := df.Times()
		mcaps := df.Column(*found, data.MarketCap)
		closes := df.Column(*found, data.MetricClose)
		vols := df.Column(*found, data.Volume)
		for i := range ts {
			fmt.Printf("  %s mcap=%v close=%v vol=%v dvol=%v\n",
				ts[i].Format("2006-01-02"), mcaps[i], closes[i], vols[i],
				closes[i]*vols[i])
		}
	}
}
