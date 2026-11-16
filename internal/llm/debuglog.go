// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"lectureflow/internal/sched"
)

// debugEntry is one line of data/logs/llm/{job_id}.jsonl (SPEC §12).
type debugEntry struct {
	Time       string            `json:"time"`
	JobID      string            `json:"job_id"`
	Role       string            `json:"role"`
	Provider   string            `json:"provider"`
	ProviderID string            `json:"provider_id,omitempty"`
	API        string            `json:"api,omitempty"` // openai | anthropic
	Model      string            `json:"model"`
	URL        string            `json:"url"`
	Attempt    int               `json:"attempt"` // 0 = first try
	Headers    map[string]string `json:"request_headers"`
	Request    json.RawMessage   `json:"request"`
	Status     int               `json:"status,omitempty"`
	Response   json.RawMessage   `json:"response,omitempty"`
	Error      string            `json:"error,omitempty"`
	DurationMS int64             `json:"duration_ms"`
}

// debugLog appends request and response to {logDir}/{job_id}.jsonl when
// log_level = debug. Old files are removed with the call log's (calllog.go,
// pruneLocked: CallLogKeepDays, CallLogMaxBytes; by the day a file was
// last written). Authentication headers (Authorization, x-api-key) are
// never written, and a key echoed in the response or the error is redacted
// (sec2-app L1).
func (c *HTTPClient) debugLog(ctx context.Context, call callInfo, h http.Header, reqBody []byte, attempt, status int, respBody []byte, err error, d time.Duration) {
	if c.cfg.LogLevel != "debug" {
		return
	}
	jobID := debugNoJob
	if j, ok := sched.JobFrom(ctx); ok && j.ID != "" {
		jobID = j.ID
	}
	hdr := map[string]string{}
	for k, v := range h {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "x-api-key") {
			continue
		}
		hdr[k] = strings.Join(v, ", ")
	}
	e := debugEntry{
		Time:       time.Now().UTC().Format(time.RFC3339Nano),
		JobID:      jobID,
		Role:       call.role,
		Provider:   call.providerName,
		ProviderID: call.provider,
		API:        call.api,
		Model:      call.model,
		URL:        call.url,
		Attempt:    attempt,
		Headers:    hdr,
		Request:    json.RawMessage(reqBody),
		Status:     status,
		DurationMS: d.Milliseconds(),
	}
	if len(respBody) > 0 {
		// A key is plain ASCII without JSON metacharacters: replacing it
		// keeps a JSON body valid.
		respBody = []byte(redact(string(respBody), call.key))
		if json.Valid(respBody) {
			e.Response = json.RawMessage(respBody)
		} else {
			q, _ := json.Marshal(string(respBody))
			e.Response = q
		}
	}
	if err != nil {
		e.Error = redact(err.Error(), call.key)
	}
	line, mErr := json.Marshal(e)
	if mErr != nil {
		slog.Warn("llm debug log", "err", mErr)
		return
	}
	line = append(line, '\n')

	// debugMu of the call log: its pruning (calllog.go, pruneLocked) also
	// removes old debug log files and must not remove one being written.
	mu := &c.logMu
	if c.calls != nil {
		mu = &c.calls.debugMu
	}
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		slog.Warn("llm debug log", "err", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(c.logDir, filepath.Base(jobID)+debugLogExt), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Warn("llm debug log", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		slog.Warn("llm debug log", "err", err)
	}
}

const (
	debugLogExt = ".jsonl"
	// debugNoJob is the name of the debug log of requests outside a job.
	debugNoJob = "nojob"
)

// isDebugLogName reports whether name is one the debug log writes: a job
// id (a ULID, db.NewID) or debugNoJob, then ".jsonl". The call log's
// pruning (calllog.go) removes old files of these names only (T-PERF-08):
// they hold prompts and answers, and nothing else of the folder may go.
func isDebugLogName(name string) bool {
	id, ok := strings.CutSuffix(name, debugLogExt)
	if !ok {
		return false
	}
	if id == debugNoJob {
		return true
	}
	if len(id) != ulid.EncodedSize || id != strings.ToUpper(id) {
		return false
	}
	_, err := ulid.ParseStrict(id)
	return err == nil
}
