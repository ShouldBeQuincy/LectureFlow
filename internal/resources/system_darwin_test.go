// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package resources

import (
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/devices"
)

// mac-opti M6: the real Mach readers on this Mac (no fakes: they only read
// the system's counters).

func TestMacCPUReader(t *testing.T) {
	r, err := newSystemReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first, err := r.Read()
	if err != nil || first.CPU != nil || first.Zones != nil {
		t.Fatalf("first read %+v %v (the CPU primes)", first, err)
	}
	// The idle cores' ticks are summed lazily: within the sampling interval
	// (2 s) a value comes.
	var second sysReading
	for end := time.Now().Add(DefaultInterval); second.CPU == nil && time.Now().Before(end); {
		time.Sleep(100 * time.Millisecond)
		if second, err = r.Read(); err != nil {
			t.Fatal(err)
		}
	}
	if second.CPU == nil || *second.CPU < 0 || *second.CPU > 100 {
		t.Fatalf("no CPU value within %v: %v", DefaultInterval, second.CPU)
	}
	t.Logf("CPU %.1f %%", *second.CPU)
	// A read without a new tick repeats the last value (never null again).
	for range 5 {
		if again, err := r.Read(); err != nil || again.CPU == nil {
			t.Fatalf("read after a value: %v %v", again.CPU, err)
		}
	}
}

// sysctl -n hw.memsize gives the same number (the padded sysctl bytes).
func TestMacMemStatus(t *testing.T) {
	total, avail, err := readMemStatus()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		t.Fatal(err)
	}
	want, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || total != want {
		t.Fatalf("total %d, sysctl says %s", total, out)
	}
	if avail >= total {
		t.Fatalf("nothing in use: avail %d of %d", avail, total)
	}
	used := total - avail
	// vm_stat's pages of the same measure, read right after: within 10 %
	// of the total (the system keeps running in between).
	vm, page := vmStatPages(t)
	ref := devices.MacMemUsedBytes(vm, page)
	diff := float64(max(used, ref) - min(used, ref))
	if diff > 0.10*float64(total) {
		t.Fatalf("used %d bytes, vm_stat %d", used, ref)
	}
	t.Logf("used %.2f GiB of %.2f GiB (vm_stat %.2f GiB)", float64(used)/(1<<30), float64(total)/(1<<30), float64(ref)/(1<<30))
}

var vmStatLine = regexp.MustCompile(`^(.+?):\s+(\d+)\.$`)

func vmStatPages(t *testing.T) (devices.MacVM, uint64) {
	t.Helper()
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
	m := regexp.MustCompile(`page size of (\d+) bytes`).FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("vm_stat header %q", lines[0])
	}
	page, _ := strconv.ParseUint(m[1], 10, 64)
	vals := map[string]uint64{}
	for _, l := range lines[1:] {
		if m := vmStatLine.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			v, _ := strconv.ParseUint(m[2], 10, 64)
			vals[strings.Trim(m[1], `"`)] = v
		}
	}
	return devices.MacVM{Internal: vals["Anonymous pages"], Purgeable: vals["Pages purgeable"], Wire: vals["Pages wired down"],
		Compressor: vals["Pages occupied by compressor"]}, page
}

func TestMacPlatformReadings(t *testing.T) {
	unsupported, unified := platformReadings()
	if !slices.Equal(unsupported, []string{ReadingCPUTemp}) || unified != (runtime.GOARCH == "arm64") {
		t.Fatalf("%v %v", unsupported, unified)
	}
}

// ioreg on this Mac gives the GPU's readings. A virtual machine (GitHub's
// hosted macOS CI runners) has a paravirtual GPU whose IOAccelerator reports
// the memory but no "Device Utilization %"; the card then leaves the usage
// tile out (TestAppleGPUCardUnreadable). Real Apple silicon gives both.
func TestMacIOReg(t *testing.T) {
	out, err := exec.Command(ioregExe, IORegArgs...).Output()
	if err != nil {
		t.Fatal(err)
	}
	util, mem := ParseIOAccelerator(string(out))
	vm := false
	if v, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.hv_vmm_present").Output(); err == nil {
		vm = strings.TrimSpace(string(v)) == "1"
	}
	switch {
	case runtime.GOARCH != "arm64":
	case vm && mem == nil:
		t.Fatalf("no GPU memory in ioreg (virtual machine): util %v mem %v", intOrNil(util), intOrNil(mem))
	case !vm && (util == nil || mem == nil):
		t.Fatalf("no GPU readings in ioreg: util %v mem %v", intOrNil(util), intOrNil(mem))
	}
	t.Logf("GPU %v %%, %v MB (virtual machine: %v)", intOrNil(util), intOrNil(mem), vm)
}
