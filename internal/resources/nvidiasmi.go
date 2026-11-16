// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/procutil"
)

// QueryArgs are the nvidia-smi arguments of A10.1.
var QueryArgs = []string{
	"--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit",
	"--format=csv,noheader,nounits",
}

const queryColumns = 7

// cmdTimeout bounds one nvidia-smi run (it normally answers in ~100 ms).
const cmdTimeout = 5 * time.Second

// Sample is one parsed nvidia-smi reading of the first GPU. Pointer fields
// are nil when the column was not reported ("[N/A]", "[Not Supported]", …).
type Sample struct {
	Name        string
	UtilPct     *int
	MemUsedMB   *int
	MemTotalMB  *int
	TempC       *int
	PowerW      *float64
	PowerLimitW *float64
}

// ParseQuery parses the output of nvidia-smi with QueryArgs. Only the first
// non-empty line (the first GPU) is used. A column that nvidia-smi could not
// read ("[N/A]", "[Not Supported]", "[Unknown Error]", "N/A") becomes nil;
// any other unexpected output is an error.
func ParseQuery(out string) (Sample, error) {
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	if line == "" {
		return Sample{}, errors.New("nvidia-smi: empty output")
	}
	parts := strings.Split(line, ",")
	if len(parts) != queryColumns {
		return Sample{}, fmt.Errorf("nvidia-smi: unexpected output %s", quoteShort(line))
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	s := Sample{Name: parts[0]}
	ints := []**int{&s.UtilPct, &s.MemUsedMB, &s.MemTotalMB}
	for i, dst := range ints {
		v, err := parseInt(parts[1+i])
		if err != nil {
			return Sample{}, fmt.Errorf("nvidia-smi: unexpected output %s", quoteShort(line))
		}
		*dst = v
	}
	var err error
	if s.TempC, err = parseInt(parts[4]); err != nil {
		return Sample{}, fmt.Errorf("nvidia-smi: unexpected output %s", quoteShort(line))
	}
	if s.PowerW, err = parseFloat(parts[5]); err != nil {
		return Sample{}, fmt.Errorf("nvidia-smi: unexpected output %s", quoteShort(line))
	}
	if s.PowerLimitW, err = parseFloat(parts[6]); err != nil {
		return Sample{}, fmt.Errorf("nvidia-smi: unexpected output %s", quoteShort(line))
	}
	return s, nil
}

// notReported matches the placeholders nvidia-smi prints for a value it
// cannot read: "[N/A]", "[Not Supported]", "[Unknown Error]", "[GPU is lost]"
// and the bare "N/A" of some driver versions.
func notReported(v string) bool {
	return v == "" || v == "N/A" || (strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"))
}

func parseFloat(v string) (*float64, error) {
	if notReported(v) {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("not a number: %q", v)
	}
	return &f, nil
}

// parseInt accepts "37" and, defensively, "37.0" (rounded).
func parseInt(v string) (*int, error) {
	if notReported(v) {
		return nil, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return &n, nil
	}
	f, err := parseFloat(v)
	if err != nil {
		return nil, err
	}
	n := int(math.Round(*f))
	return &n, nil
}

// quoteShort quotes (and escapes, for garbled bytes) at most 120 bytes of s.
func quoteShort(s string) string {
	const max = 120
	if len(s) > max {
		return strconv.Quote(s[:max]) + "…"
	}
	return strconv.Quote(s)
}

// execRun is the default Deps.RunCmd: it runs the command (nvidia-smi;
// ioreg on macOS) in its own directory, not the service's current one
// (procutil.CommandInExeDir; SEC52 info-7), and returns its stdout; a
// failing exit carries the first line of stderr in the error.
func execRun(ctx context.Context, name string, args ...string) (string, error) {
	out, err := procutil.CommandInExeDir(ctx, name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg := firstLine(string(ee.Stderr))
			if msg == "" {
				msg = firstLine(string(out)) // nvidia-smi prints its errors on stdout
			}
			if msg != "" {
				return string(out), fmt.Errorf("%w: %s", err, quoteShort(msg))
			}
		}
		return string(out), err
	}
	return string(out), nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
