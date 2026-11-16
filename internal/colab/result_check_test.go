// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// A Colab result.json goes through the SSH path's check (remote.
// ValidateResult) before it is written (security audit AUD4 L-1): times
// out of range or out of order, too many sentences and the like refuse the
// whole result, control and direction characters are removed, unknown keys
// dropped; a result larger than allowed is refused instead of truncated.
// All on the stateful fake: harmless marker values only.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lectureflow/internal/remote"
)

// sameResult: the result.json written (got) holds what the worker wrote
// (want). The check re-encodes a result (key order, number spelling), so
// the bytes may differ; the content of a result within the limits may not.
func sameResult(got []byte, want string) bool {
	var g, w any
	return json.Unmarshal(got, &g) == nil && json.Unmarshal([]byte(want), &w) == nil && reflect.DeepEqual(g, w)
}

// resultOf runs lecture l1 with the fake's worker writing result (and
// reporting declared bytes, 0 = the real size).
func resultOf(t *testing.T, e *fakeEnv, result string, declared int64) (path string, err error) {
	t.Helper()
	e.f.mu.Lock()
	e.f.result, e.f.declaredBytes = result, declared
	e.f.mu.Unlock()
	return e.b.Transcribe(context.Background(), e.lecture(t, "l1"), nil)
}

// A result refused: the lecture goes back as a contract violation, nothing
// is written, the download is not tried again, and the runtime is released
// as after any other violation (no wait, no further compute).
func assertResultRefused(t *testing.T, e *fakeEnv, err error, reasonPart string, gets int) {
	t.Helper()
	if !IsFallback(err) || Code(err) != CodeContractViolation {
		t.Fatalf("err %v (code %s), want a hand-back with %s", err, Code(err), CodeContractViolation)
	}
	if r, _ := codedParam(err, "reason"); !strings.Contains(r, reasonPart) {
		t.Errorf("reason %q, want it to say %q", r, reasonPart)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, "l1", "result.json")); serr == nil {
		t.Error("a refused result was written")
	}
	if n := e.f.count(func() int { return e.f.resultGets }); n != gets {
		t.Errorf("result.json was downloaded %d times, want %d", n, gets)
	}
	if got := e.sleeps(); len(got) != 0 {
		t.Errorf("waits %v, want none: a refused result is not retried", got)
	}
	e.b.handbacksSettled()
	if st := e.b.Status(context.Background()); !e.f.wasReleased() || st.LastRelease == nil || st.LastRelease.Reason != ReleaseFailed {
		t.Errorf("released %v, last release %+v, want released as failed", e.f.wasReleased(), st.LastRelease)
	}
}

// What the audit's probe showed the old check accepting.
func TestColabResultOutOfRangeIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, result, reason string
	}{
		{"audit probe", `{"sentences":[` +
			`{"start_ms":-5000,"end_ms":99999999999,"text":"‮\u001b[31mmarker\u0007","low_conf_words":[{"i":-9223372036854775808,"w":"x"}]},` +
			`{"start_ms":1000,"end_ms":2000,"text":"ok"}]}`, "start_ms"},
		{"beyond the audio", `{"sentences":[{"start_ms":0,"end_ms":900000,"text":"marker","low_conf_words":[]}]}`, "end_ms"},
		{"out of order", `{"sentences":[{"start_ms":1000,"end_ms":2000,"text":"a","low_conf_words":[]},` +
			`{"start_ms":10,"end_ms":20,"text":"b","low_conf_words":[]}]}`, "before the previous"},
		{"too many sentences", `{"sentences":[` + strings.Repeat(`{"start_ms":0,"end_ms":1,"text":"m","low_conf_words":[]},`, remote.MaxResultSentences) +
			`{"start_ms":0,"end_ms":1,"text":"m","low_conf_words":[]}]}`, "too many"},
		{"not a result", `{"sentences":"marker"}`, "sentences"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeEnv(t, t4())
			_, err := resultOf(t, e, tc.result, 0)
			assertResultRefused(t, e, err, tc.reason, 1)
		})
	}
}

// A result within the limits is written as the check re-encodes it:
// control and direction characters gone, unknown keys dropped, the
// statistics the lecture page shows kept — 极速模式's cuts included, which
// the shared check alone would drop (an array in timing).
func TestColabResultIsWrittenChecked(t *testing.T) {
	const timing = `"timing":{"load_s":1.5,"transcribe_s":60,"audio_s":600,"first_compile":false,"peak_working_set_mb":0,` +
		`"postprocess":{"redecode":{"skipped":false,"windows":20,"unpunctuated":1,"uppercase":0,"redecoded":1,"accepted":1,"redecode_s":2.5},` +
		`"textrepair":{"skipped":"","version":1,"words":100},"split":{"parts":2,"cuts_ms":%s}}}`
	result := func(cuts string) string {
		return `{"prompt_terms_used":2,"marker_key":"x","sentences":[` +
			`{"start_ms":0,"end_ms":500,"text":"a‮b\u001b[31m c\u0007","low_conf_words":[{"i":0,"w":"a‏b"}],"words":[{"w":"a","s":0,"e":500}]},` +
			`{"start_ms":400000,"end_ms":401000,"text":"y","low_conf_words":[]}],` +
			strings.Replace(timing, "%s", cuts, 1) + `}`
	}
	type written struct {
		PromptTermsUsed int                        `json:"prompt_terms_used"`
		Sentences       []remote.ResultSentence    `json:"sentences"`
		Timing          map[string]json.RawMessage `json:"timing"`
	}
	// read returns the written result, its top-level keys and
	// timing.postprocess.
	read := func(t *testing.T, path string) (written, map[string]json.RawMessage, map[string]map[string]any) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var w written
		var top map[string]json.RawMessage
		var post map[string]map[string]any
		if json.Unmarshal(raw, &w) != nil || json.Unmarshal(raw, &top) != nil || json.Unmarshal(w.Timing["postprocess"], &post) != nil {
			t.Fatalf("result.json written: %s", raw)
		}
		return w, top, post
	}

	t.Run("cleaned, statistics kept", func(t *testing.T) {
		e := newFakeEnv(t, t4())
		path, err := resultOf(t, e, result("[300000]"), 0)
		if err != nil {
			t.Fatalf("Transcribe: %v (code %s)", err, Code(err))
		}
		w, top, post := read(t, path)
		if _, ok := top["marker_key"]; ok {
			t.Error("an unknown key was kept")
		}
		if w.PromptTermsUsed != 2 || len(w.Sentences) != 2 {
			t.Errorf("prompt_terms_used %d, %d sentences", w.PromptTermsUsed, len(w.Sentences))
		}
		txt := w.Sentences[0].Text + w.Sentences[0].LowConfWords[0].W
		if strings.ContainsAny(txt, "‮‏\u001b\u0007") || !strings.HasPrefix(w.Sentences[0].Text, "ab") {
			t.Errorf("text %q, low-confidence word %q: control and direction characters must be gone", w.Sentences[0].Text, w.Sentences[0].LowConfWords[0].W)
		}
		if len(w.Sentences[0].Words) != 1 || w.Sentences[1].StartMS != 400000 {
			t.Errorf("sentences %+v", w.Sentences)
		}
		split := post["split"]
		cuts, _ := split["cuts_ms"].([]any)
		if split["parts"] != float64(2) || len(cuts) != 1 || cuts[0] != float64(300000) {
			t.Errorf("split %v, want 2 parts cut at 300000 ms", split)
		}
		if post["redecode"]["accepted"] != float64(1) || post["textrepair"]["version"] != float64(1) {
			t.Errorf("postprocess %v: the counters must stay", post)
		}
		e.b.removalsSettled()
		if e.f.wasReleased() {
			t.Error("the runtime was released after a good result")
		}
	})
	t.Run("cuts outside the audio are not kept", func(t *testing.T) {
		e := newFakeEnv(t, t4())
		path, err := resultOf(t, e, result("[900000]"), 0)
		if err != nil {
			t.Fatalf("Transcribe: %v (code %s)", err, Code(err))
		}
		_, _, post := read(t, path)
		if cuts, ok := post["split"]["cuts_ms"]; ok {
			t.Errorf("cuts %v kept although they lie beyond the 600 s lecture", cuts)
		}
		e.b.removalsSettled()
	})
}

// Larger than allowed: refused with a clear reason, never truncated.
func TestColabResultLargerThanAllowedIsRefused(t *testing.T) {
	t.Run("declared by the done event", func(t *testing.T) {
		// not downloaded at all
		e := newFakeEnv(t, t4())
		_, err := resultOf(t, e, "", remote.MaxResultBytes+1)
		assertResultRefused(t, e, err, "larger", 0)
	})
	t.Run("the answer itself", func(t *testing.T) {
		// the done event reports a small file; the answer is bigger than
		// any result may be: the download stops at its limit
		e := newFakeEnv(t, t4())
		big := `{"sentences":[{"start_ms":0,"end_ms":1,"text":"` + strings.Repeat("m", int(maxResultAnswer)/4*3+(1<<20)) + `"}]}`
		_, err := resultOf(t, e, big, 100)
		assertResultRefused(t, e, err, "larger", 1)
	})
}

// download stops reading at its limit and says so: an answer one byte over
// is errAnswerTooLarge, not a truncated contents model.
func TestDownloadAnswerOverItsLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","format":"text","content":"`))
		chunk := []byte(strings.Repeat("m", 1<<16))
		for n := int64(0); n <= maxResultAnswer; n += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	rt := &runtimeClient{hc: srv.Client(), token: "marker", agent: "t"}
	rt.base, _ = url.Parse(srv.URL + "/rt")
	_, err := rt.download(context.Background(), "content/lectureflow/jobs/x/result.json")
	if !errors.Is(err, errAnswerTooLarge) {
		t.Fatalf("err %v, want %v", err, errAnswerTooLarge)
	}
	if want := int64(remote.MaxResultBytes) * 4 / 3; maxResultAnswer < want || maxResultAnswer > 2*want {
		t.Errorf("limit %d: it must hold a %d-byte result in base64 with room for the contents model, and not much more",
			maxResultAnswer, remote.MaxResultBytes)
	}
}
