// Copyright 2026
// SPDX-License-Identifier: Apache-2.0
//
// Diagnostic-only: probes whether GOOG and GOOGL are both present in pvbt's
// us-tradable universe and what mcap pvbt assigns each.

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

	for _, ticker := range []string{"GOOG", "GOOGL"} {
		la, le := provider.LookupAsset(ctx, ticker)
		fmt.Printf("\n=== %s ===\n", ticker)
		fmt.Printf("LookupAsset err=%v ticker=%s figi=%s name=%q listed=%s delisted=%s\n",
			le, la.Ticker, la.CompositeFigi, la.Name,
			la.Listed.Format("2006-01-02"), la.Delisted.Format("2006-01-02"))
	}

	dates := []time.Time{
		time.Date(2015, 1, 2, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 7, 1, 16, 0, 0, 0, time.UTC),
		time.Date(2015, 12, 31, 16, 0, 0, 0, time.UTC),
	}
	u := universe.NewIndex(provider, "us-tradable")

	for _, d := range dates {
		fmt.Printf("\n=== %s ===\n", d.Format("2006-01-02"))
		members := u.Assets(d)

		var goog, googl *asset.Asset
		for i, m := range members {
			if m.Ticker == "GOOG" {
				ms := m
				goog = &ms
			}
			if m.Ticker == "GOOGL" {
				ms := m
				googl = &ms
			}
			_ = i
		}
		fmt.Printf("  GOOG in universe: %v\n", goog != nil)
		fmt.Printf("  GOOGL in universe: %v\n", googl != nil)

		// Fetch mcap for both regardless of universe membership.
		for _, ticker := range []string{"GOOG", "GOOGL"} {
			la, le := provider.LookupAsset(ctx, ticker)
			if le != nil {
				continue
			}
			df, ferr := provider.Fetch(ctx, data.DataRequest{
				Assets:  []asset.Asset{la},
				Metrics: []data.Metric{data.MarketCap, data.MetricClose, data.Volume},
				Start:   d.AddDate(0, 0, -3),
				End:     d,
			})
			if ferr != nil {
				fmt.Printf("  %s fetch err: %v\n", ticker, ferr)
				continue
			}
			ts := df.Times()
			mcaps := df.Column(la, data.MarketCap)
			closes := df.Column(la, data.MetricClose)
			vols := df.Column(la, data.Volume)
			for i := range ts {
				dvol := closes[i] * vols[i]
				fmt.Printf("    %s %s mcap=$%.1fB close=%.2f vol=%.0f dvol=$%.1fM\n",
					ts[i].Format("2006-01-02"), ticker, mcaps[i]/1e9, closes[i], vols[i], dvol/1e6)
			}
		}
	}
}
