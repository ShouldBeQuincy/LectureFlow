// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// T16 D5 (r4): the diagnostics page's device table scrolls sideways in its
// wrapper on a narrow screen; the wrapper is in the Tab order (so the arrow
// keys scroll it) as a region named by the card's title, the GPU group's
// name without its count. No new text: the name is the title already shown.
// R48 M1: the wrapper has a stable fkey, so preserveFocus keeps the focus
// on it when the page is drawn anew.
func TestSystemDeviceTableScrollable(t *testing.T) {
	src := readWeb(t, "view_system.js")
	for _, want := range []string{
		"const DEVICES_TITLE_ID = 'sys-devices-title';",
		"export const DEVICE_TABLE_FKEY = 'sys:dev:table';",
		"h('span', { id: DEVICES_TITLE_ID }, t('system.group.gpu')), devs.length ? h('span', { class: 'card-count' }",
		"return h('div', { class: 'sys-dev-wrap', tabindex: '0', role: 'region', 'aria-labelledby': DEVICES_TITLE_ID, dataset: { fkey: DEVICE_TABLE_FKEY } }, table);",
		"? deviceTableWrap(\n",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_system.js lacks %q", want)
		}
	}
	// one wrapper and one title: the id names exactly one element
	if n := strings.Count(src, "class: 'sys-dev-wrap'"); n != 1 {
		t.Errorf("view_system.js has %d device table wrappers, want 1", n)
	}
	if n := strings.Count(src, "deviceTableWrap(") - strings.Count(src, "function deviceTableWrap("); n != 1 {
		t.Errorf("view_system.js draws the device table wrapper %d times, want 1", n)
	}
	if n := len(regexp.MustCompile(`\{ id: DEVICES_TITLE_ID \}`).FindAllString(src, -1)); n != 1 {
		t.Errorf("view_system.js gives the title id to %d elements, want 1", n)
	}
	if n := strings.Count(src, "devicesCard(sys)") - strings.Count(src, "function devicesCard(sys)"); n != 1 {
		t.Errorf("the devices card is drawn %d times on the page, want 1 (its title id must stay unique)", n)
	}
	// the fkey is unique in the view
	if n := strings.Count(src, "'sys:dev:table'"); n != 1 {
		t.Errorf("view_system.js uses the key 'sys:dev:table' %d times, want 1", n)
	}
}

// The device card's behaviour (testdata/sys_devices_test.mjs): the
// wrapper keeps the focus across a re-render, and a VRAM hint's sentences
// are joined by their end mark (zh-CN, en, hi).
func TestSystemDeviceCardLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/sys_devices_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "sys_devices_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "sys devices checks: ok") {
		t.Fatalf("sys_devices_test.mjs: %v\n%s", err, out)
	}
}
