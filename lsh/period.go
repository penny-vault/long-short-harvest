// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh

import "github.com/penny-vault/pvbt/portfolio"

// tradingDays converts a count of trading days to a calendar-day pvbt Period
// generous enough to span at least that many bars. Uses 7/5 for the
// trading-to-calendar ratio (slightly conservative versus 365/252) plus a
// 30-day weekend/holiday buffer.
func tradingDays(n int) portfolio.Period {
	if n < 0 {
		n = 0
	}
	return portfolio.Days(n*7/5 + 30)
}
