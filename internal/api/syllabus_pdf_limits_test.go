// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The syllabus PDF extraction's limits (security review 2, M2): the same
// memory cap, decode budget and time limit as the material stage
// (stages/pdf_limits.go), reported as coded 422 errors.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/pyenv"
	"lectureflow/internal/stages"
)

// fakePDFLimitsEnv makes the test binary act as extract_pdf.py (TestMain):
//
//	budget    the worker's decode-budget error (ERROR_CODE, exit 2)
//	hang      "STATUS pages 1", then sleeps
//	joblimit  one page "job <job.json memory_limit_bytes> limit <the OS job's memory limit>"
//	env       one page "telemetry_off <pyenv.Has(its environment)>" (pyenv_test.go)
const fakePDFLimitsEnv = "LECTUREFLOW_TEST_FAKE_PDF_LIMITS"

func fakePDFLimitsWorker(mode string, args []string) int {
	in, out := "", ""
	for i, a := range args {
		if a == "--in" && i+1 < len(args) {
			in = args[i+1]
		}
		if a == "--out" && i+1 < len(args) {
			out = args[i+1]
		}
	}
	switch mode {
	case "budget":
		fmt.Println("STATUS pages 3")
		fmt.Println(`ERROR_CODE decode_budget {"limit_bytes": 4194304}`)
		fmt.Fprintln(os.Stderr, "ERROR: input error: the PDF's streams decode to more than 4194304 bytes (decode budget)")
		return 2
	case "hang":
		fmt.Println("STATUS pages 1")
		time.Sleep(30 * time.Second)
		return 0
	case "joblimit":
		var job struct {
			MemoryLimit int64 `json:"memory_limit_bytes"`
		}
		b, err := os.ReadFile(in)
		if err != nil || json.Unmarshal(b, &job) != nil {
			return 4
		}
		page := fmt.Sprintf("job %d limit %d", job.MemoryLimit, ownJobMemoryLimit())
		res, _ := json.Marshal(map[string]any{"pages": []string{page}})
		if os.WriteFile(out, res, 0o644) != nil {
			return 5
		}
		return 0
	case "env":
		res, _ := json.Marshal(map[string]any{"pages": []string{fmt.Sprintf("telemetry_off %v", pyenv.Has(os.Environ()))}})
		if os.WriteFile(out, res, 0o644) != nil {
			return 5
		}
		return 0
	case "argsenv": // system_isolation_test.go: the arguments and the environment, JSON
		b, _ := json.Marshal(map[string][]string{"args": args, "env": os.Environ()})
		if out == "" { // a diagnostics command (runCommand): on stdout
			fmt.Println(string(b))
			return 0
		}
		res, _ := json.Marshal(map[string]any{"pages": []string{string(b)}})
		if os.WriteFile(out, res, 0o644) != nil {
			return 5
		}
		return 0
	}
	return 3
}

func pdfLimitsEnv(t *testing.T, mode string) *env {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakePDFLimitsEnv, mode)
	return newEnvWith(t, envConfig{server: func(s *Server) {
		s.cfg.Components = pdfLocator{exe: exe}
		s.extractPDF = s.runExtractPDF // the real extraction (helpers fake it)
	}})
}

func uploadSyllabusPDF(t *testing.T, e *env) ErrorBody {
	t.Helper()
	c := e.course("C")
	res, b := e.upload("/api/courses/"+c.ID+"/syllabus", nil, "outline.pdf", []byte("%PDF-1.4 fake"))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var er ErrorBody
	if err := json.Unmarshal(b, &er); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return er
}

// Over the worker's decode budget: 422 pdf.decode_budget {limit_bytes}.
func TestSyllabusPDFDecodeBudget(t *testing.T) {
	e := pdfLimitsEnv(t, "budget")
	er := uploadSyllabusPDF(t, e)
	if er.Error.Code != stages.CodePDFDecodeBudget || er.Error.Params["limit_bytes"] != float64(4194304) {
		t.Fatalf("error %+v", er.Error)
	}
}

// Past the time limit (capped at syllabusPDFTimeout): the worker is killed,
// 422 pdf.timeout {minutes}.
func TestSyllabusPDFTimeout(t *testing.T) {
	old := syllabusPDFTimeout
	syllabusPDFTimeout = 300 * time.Millisecond
	t.Cleanup(func() { syllabusPDFTimeout = old })
	e := pdfLimitsEnv(t, "hang")
	start := time.Now()
	er := uploadSyllabusPDF(t, e)
	if er.Error.Code != stages.CodePDFTimeout || er.Error.Params["minutes"] != float64(1) {
		t.Fatalf("error %+v", er.Error)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %v", d)
	}
}

// job.json carries the memory limit, and on Windows the process runs in a
// job with that cap.
func TestSyllabusPDFMemoryLimit(t *testing.T) {
	e := pdfLimitsEnv(t, "joblimit")
	work := t.TempDir()
	pdf := filepath.Join(work, "syllabus.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	pages, err := e.srv.runExtractPDF(context.Background(), work, pdf)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("job %d limit %d", stages.PDFMemoryLimit, wantOSJobLimit(stages.PDFMemoryLimit))
	if len(pages) != 1 || pages[0] != want {
		t.Fatalf("pages %q, want %q", pages, want)
	}
}
