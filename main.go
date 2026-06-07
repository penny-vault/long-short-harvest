// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/penny-vault/long-short-harvest/lsh"
	"github.com/penny-vault/pvbt/cli"
)

func main() {
	cli.Run(&lsh.LongShortHarvest{})
}
