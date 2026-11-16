// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"net/netip"
	"os"
	"testing"
)

// ipClassTable is testdata/ipclass.json: the address classes this
// package's ipReach and internal/fetch's classifyIP share (SEC47 info-2).
// internal/fetch/ipclass_test.go reads the same table.
type ipClassTable struct {
	Both     map[string]string `json:"both"`
	Stricter map[string]struct {
		Components string `json:"components"`
		Fetch      string `json:"fetch"`
	} `json:"components_stricter"`
}

func readIPClassTable(t *testing.T) ipClassTable {
	t.Helper()
	b, err := os.ReadFile("../../testdata/ipclass.json")
	if err != nil {
		t.Fatal(err)
	}
	var tab ipClassTable
	if err := json.Unmarshal(b, &tab); err != nil {
		t.Fatal(err)
	}
	if len(tab.Both) < 60 || len(tab.Stricter) < 5 {
		t.Fatalf("the table has %d + %d addresses", len(tab.Both), len(tab.Stricter))
	}
	return tab
}

// componentsReach is a class of the table as this package's reach.
var componentsReach = map[string]reach{
	"public": reachPublic, "localnet": reachLocalNet, "loopback": reachLoopback,
	"unsafe": reachNever, "notunicast": reachNever, "never": reachNever,
}

// ipReach says what the shared table says, for every address; the
// differences with internal/fetch are the table's, and components is the
// stricter one in each.
func TestIPReachMatchesSharedTable(t *testing.T) {
	tab := readIPClassTable(t)
	check := func(addr, class string) {
		want, ok := componentsReach[class]
		if !ok {
			t.Errorf("%s: unknown class %q", addr, class)
			return
		}
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			t.Errorf("%s: %v", addr, err)
			return
		}
		if got := ipReach(ip); got != want {
			t.Errorf("ipReach(%s) = %d, the table says %s (%d)", addr, got, class, want)
		}
	}
	for addr, class := range tab.Both {
		check(addr, class)
	}
	for addr, d := range tab.Stricter {
		check(addr, d.Components)
		if f, ok := componentsReach[d.Fetch]; !ok || f >= componentsReach[d.Components] {
			t.Errorf("%s: components %s is not stricter than fetch %s", addr, d.Components, d.Fetch)
		}
		if _, dup := tab.Both[addr]; dup {
			t.Errorf("%s is in both lists", addr)
		}
	}
}
