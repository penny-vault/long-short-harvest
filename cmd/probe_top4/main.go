// Copyright 2026
// SPDX-License-Identifier: Apache-2.0
//
// Diagnostic-only: prints pvbt's top-10 by market cap on a few 2015 dates so we
// can compare against QC's top-4 picks (which use GOOG, AAPL, MSFT, XOM in 2015
// based on the orders log).

package main

import (
	"context"
	"fmt"
	"sort"
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

	dates := []time.Time{
		time.Date(2015, 1, 2, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 2, 3, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 3, 3, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 4, 1, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 5, 1, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 6, 2, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 7, 1, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 8, 3, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 9, 1, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 10, 1, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 11, 3, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 12, 1, 16, 0, 0, 0, time.UTC),
	}

	u := universe.NewIndex(provider, "us-tradable")

	for _, d := range dates {
		members := u.Assets(d)
		fmt.Printf("\n=== top-mcap on %s (universe size %d) ===\n", d.Format("2006-01-02"), len(members))

		mcapDF, err := provider.Fetch(ctx, data.DataRequest{
			Assets:  members,
			Metrics: []data.Metric{data.MarketCap},
			Start:   d.AddDate(0, 0, -1),
			End:     d,
		})
		if err != nil {
			fmt.Printf("  fetch err: %v\n", err)
			continue
		}

		type rank struct {
			a    asset.Asset
			mcap float64
		}
		ranks := make([]rank, 0, len(members))
		for _, m := range members {
			col := mcapDF.Column(m, data.MarketCap)
			if len(col) == 0 {
				continue
			}
			last := col[len(col)-1]
			if last <= 0 {
				continue
			}
			ranks = append(ranks, rank{a: m, mcap: last})
		}
		sort.Slice(ranks, func(i, j int) bool { return ranks[i].mcap > ranks[j].mcap })
		n := 6
		if len(ranks) < n {
			n = len(ranks)
		}
		for i := 0; i < n; i++ {
			fmt.Printf("  %2d  %-10s  $%.1fB\n",
				i+1, ranks[i].a.Ticker, ranks[i].mcap/1e9)
		}
	}
}
