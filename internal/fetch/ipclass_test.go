// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"encoding/json"
	"net/netip"
	"os"
	"testing"
)

// The address classes classifyIP shares with internal/components ipReach
// (SEC47 info-2): testdata/ipclass.json, which
// internal/components/ipclass_test.go reads too. A change of either
// classification fails until the table follows.
func TestClassifyIPMatchesSharedTable(t *testing.T) {
	b, err := os.ReadFile("../../testdata/ipclass.json")
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		Both     map[string]string `json:"both"`
		Stricter map[string]struct {
			Components string `json:"components"`
			Fetch      string `json:"fetch"`
		} `json:"components_stricter"`
	}
	if err := json.Unmarshal(b, &tab); err != nil {
		t.Fatal(err)
	}
	if len(tab.Both) < 60 || len(tab.Stricter) < 5 {
		t.Fatalf("the table has %d + %d addresses", len(tab.Both), len(tab.Stricter))
	}
	classes := map[string]addrClass{
		"public": classPublic, "localnet": classLocalNet, "loopback": classLoopback,
		"unsafe": classUnsafe, "notunicast": classNotUnicast,
	}
	check := func(addr, class string) {
		want, ok := classes[class]
		if !ok {
			t.Errorf("%s: unknown class %q", addr, class)
			return
		}
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			t.Errorf("%s: %v", addr, err)
			return
		}
		if got := classifyIP(ip); got != want {
			t.Errorf("classifyIP(%s) = %d, the table says %s (%d)", addr, got, class, want)
		}
	}
	for addr, class := range tab.Both {
		check(addr, class)
	}
	for addr, d := range tab.Stricter {
		check(addr, d.Fetch)
	}
}
