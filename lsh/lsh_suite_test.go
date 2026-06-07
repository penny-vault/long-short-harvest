// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package lsh_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLongShortHarvest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Long Short Harvest Suite")
}
