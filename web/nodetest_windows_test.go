// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package web

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The memory cap of a node run holds memory outside the JavaScript heap
// too (node's diff keeps typed arrays): a run that allocates buffers past
// it fails, and the error says so. Windows only: procutil enforces the cap
// there (a Job Object); elsewhere the timeout is the only bound.
func TestNodeMemoryCap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the guarded node runs need it")
	}
	// A smaller cap than nodeMemoryBytes, on the same path, to keep the
	// test light. The script allocates only to one step past the cap and
	// ends itself after 10 s if nothing stops it.
	const capBytes = 256 << 20
	script := fmt.Sprintf(`setTimeout(() => { console.log('not stopped'); process.exit(3); }, 10000);
const step = 16 << 20, kept = [];
let total = 0;
while (total <= %d + step) { kept.push(Buffer.alloc(step, 1)); total += step; }
console.log('allocated', total >> 20, 'MB');`, capBytes)
	start := time.Now()
	cmd := nodeCommand(t, node, 30*time.Second, "-e", script)
	out, err := combinedCapped(cmd, capBytes)
	took := time.Since(start)
	if err == nil || strings.Contains(string(out), "allocated") {
		t.Fatalf("a node run past its memory cap was not stopped (%v):\n%s", err, out)
	}
	if !strings.Contains(err.Error(), "memory cap of 256 MB") {
		t.Fatalf("the error does not name the memory cap: %v\n%s", err, out)
	}
	if took > 20*time.Second {
		t.Fatalf("stopped after %v, not at once", took)
	}
	t.Logf("%v (%v)", err, took)
}
