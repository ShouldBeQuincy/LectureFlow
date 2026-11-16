// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"regexp"
	"strings"
	"testing"
)

// T041: a provider renamed in the settings shows its new name in the
// resource panel with the next snapshot. The rows of an unchanged provider
// list are updated in place (updateProviderRow), so that update must carry
// the name too, not only the balance, the tag and the body.
func TestProviderRowUpdatesName(t *testing.T) {
	res := readWeb(t, "resources.js")
	if !strings.Contains(res, "name: h('span', { class: 'prov-name' }, p.name)") || !strings.Contains(res, "h('div', { class: 'prov-head' }, ref.name,") {
		t.Error("providerRow does not keep a reference to the name element")
	}
	m := regexp.MustCompile(`(?s)function updateProviderRow\(ref, p\) \{(.*?)\n  \}`).FindStringSubmatch(res)
	if m == nil {
		t.Fatal("resources.js has no updateProviderRow")
	}
	if !strings.Contains(m[1], "ref.name.textContent = p.name") {
		t.Error("updateProviderRow does not update the provider's name")
	}
}
