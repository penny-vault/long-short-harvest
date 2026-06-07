// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import (
	"math"
	"strings"

	"github.com/penny-vault/pvbt/asset"
)

// weightPlan accumulates per-asset target weights from both sleeves and
// records short justification fragments. It is finalized once via toMembers
// to produce the Allocation.Members map handed to RebalanceTo.
type weightPlan struct {
	members []weightedAsset
	notes   []string
}

type weightedAsset struct {
	asset  asset.Asset
	weight float64
}

func newWeightPlan() *weightPlan {
	return &weightPlan{}
}

// add records a target weight for the asset. Repeated calls overwrite prior
// entries for the same asset; the latest weight wins. NaN/Inf weights are
// dropped.
func (p *weightPlan) add(a asset.Asset, w float64) {
	if math.IsNaN(w) || math.IsInf(w, 0) {
		return
	}
	for i := range p.members {
		if p.members[i].asset == a {
			p.members[i].weight = w
			return
		}
	}
	p.members = append(p.members, weightedAsset{asset: a, weight: w})
}

// note appends a free-text fragment used to assemble the rebalance
// justification. Empty strings are ignored.
func (p *weightPlan) note(s string) {
	if s == "" {
		return
	}
	p.notes = append(p.notes, s)
}

// toMembers returns the planned weights as the map shape consumed by
// portfolio.Allocation. Zero-weight entries are dropped so RebalanceTo treats
// them as exits rather than re-emitting flat orders.
func (p *weightPlan) toMembers() map[asset.Asset]float64 {
	out := make(map[asset.Asset]float64, len(p.members))
	for _, m := range p.members {
		if m.weight == 0 {
			continue
		}
		out[m.asset] = m.weight
	}
	return out
}

// justification joins the recorded notes into a human-readable string for the
// rebalance log.
func (p *weightPlan) justification() string {
	return strings.Join(p.notes, "; ")
}
