// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestValidateResultKeepsKnownKeys(t *testing.T) {
	raw := []byte(`{"prompt_terms_used":3,"unknown_top":{"x":[1,2]},"sentences":[
		{"start_ms":0,"end_ms":1000,"text":"Hello\u0000 world` + "\u202e" + `.","low_conf_words":[{"i":1,"w":"world"}],
		 "words":[{"w":"Hello","s":0,"e":400},{"w":"world","s":420,"e":1000}],"secret":"dropped"},
		{"start_ms":1000,"end_ms":2500,"text":"Next."}],
		"timing":{"load_s":1.5,"transcribe_s":2,"audio_s":3,"postprocess":{"textrepair":{"changed":2}},"list":[1]},
		"parts":[{"from_ms":0,"to_ms":2500,"device_id":"ssh:mac","model":"large-v3-turbo","precision":"fp16"}]}`)
	out, res, err := ValidateResult(raw, 3000)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"unknown_top", "secret", "\u0000", "\u202e", `"list"`} {
		if strings.Contains(s, bad) {
			t.Fatalf("%q kept: %s", bad, s)
		}
	}
	if res.PromptTermsUsed != 3 || len(res.Sentences) != 2 || res.Sentences[0].Text != "Hello world." ||
		len(res.Sentences[0].Words) != 2 || len(res.Parts) != 1 || res.Timing["postprocess"] == nil {
		t.Fatalf("%+v", res)
	}
	// The re-encoded result reads like a worker's (asr.go's struct).
	var v struct {
		Sentences []struct {
			StartMS      int64 `json:"start_ms"`
			LowConfWords []struct {
				I int    `json:"i"`
				W string `json:"w"`
			} `json:"low_conf_words"`
		} `json:"sentences"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Sentences[0].LowConfWords[0].W != "world" {
		t.Fatalf("%v %+v", err, v)
	}
}

func TestValidateResultRefuses(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":          `nope`,
		"array":             `[]`,
		"no sentences":      `{"prompt_terms_used":1}`,
		"sentences object":  `{"sentences":{}}`,
		"past the audio":    `{"sentences":[{"start_ms":0,"end_ms":99000,"text":"x"}]}`,
		"negative":          `{"sentences":[{"start_ms":-5,"end_ms":10,"text":"x"}]}`,
		"backwards":         `{"sentences":[{"start_ms":500,"end_ms":100,"text":"x"}]}`,
		"not monotonic":     `{"sentences":[{"start_ms":900,"end_ms":1000,"text":"a"},{"start_ms":100,"end_ms":200,"text":"b"}]}`,
		"word outside":      `{"sentences":[{"start_ms":0,"end_ms":1000,"text":"a","words":[{"w":"a","s":0,"e":1900}]}]}`,
		"word reversed":     `{"sentences":[{"start_ms":0,"end_ms":1000,"text":"a","words":[{"w":"a","s":500,"e":100}]}]}`,
		"no text":           `{"sentences":[{"start_ms":0,"end_ms":10}]}`,
		"text type":         `{"sentences":[{"start_ms":0,"end_ms":10,"text":5}]}`,
		"float time":        `{"sentences":[{"start_ms":0.5,"end_ms":10,"text":"x"}]}`,
		"long text":         `{"sentences":[{"start_ms":0,"end_ms":10,"text":"` + strings.Repeat("x", MaxSentenceRunes+1) + `"}]}`,
		"deep":              `{"sentences":[],"x":` + strings.Repeat("[", 20) + strings.Repeat("]", 20) + `}`,
		"bad timing string": `{"sentences":[],"timing":{"a":"` + strings.Repeat("y", 70) + `"}}`,
		"bad part":          `{"sentences":[],"parts":[{"from_ms":0,"to_ms":1,"device_id":"$(rm)","model":"m","precision":"p"}]}`,
		"invalid utf8":      "{\"sentences\":[{\"start_ms\":0,\"end_ms\":1,\"text\":\"\xff\"}]}",
	} {
		if _, _, err := ValidateResult([]byte(raw), 3000); err == nil || !errors.Is(err, ErrResultInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	big := make([]byte, MaxResultBytes+1)
	if _, _, err := ValidateResult(big, 3000); err == nil {
		t.Error("an oversized result was accepted")
	}
}

func TestValidateResultOfTheFakeWorker(t *testing.T) {
	// The repository's fake transcription (SPEC §14) passes as it is.
	raw, err := os.ReadFile("../../testdata/fake_asr.json")
	if err != nil {
		t.Fatal(err)
	}
	// with the fake recording's length (60 s, its last sentence ends there);
	// a length of 0 is refused since SEC41 L4
	if _, res, err := ValidateResult(raw, 60000); err != nil || len(res.Sentences) == 0 {
		t.Fatalf("%v", err)
	}
}

func FuzzValidateResult(f *testing.F) {
	f.Add([]byte(`{"sentences":[{"start_ms":0,"end_ms":10,"text":"x","words":[{"w":"x","s":0,"e":10}]}]}`), int64(1000))
	f.Add([]byte(`{"sentences":[],"timing":{"a":{"b":{"c":1}}}}`), int64(0))
	if raw, err := os.ReadFile("../../testdata/fake_asr.json"); err == nil {
		f.Add(raw, int64(0))
	}
	f.Fuzz(func(t *testing.T, raw []byte, ms int64) {
		out, res, err := ValidateResult(raw, ms)
		if err != nil {
			return
		}
		// What passes is valid JSON, valid again, and within the limits.
		out2, res2, err := ValidateResult(out, ms)
		if err != nil {
			t.Fatalf("a checked result does not check again: %v", err)
		}
		if string(out2) != string(out) || len(res2.Sentences) != len(res.Sentences) {
			t.Fatal("checking is not stable")
		}
		if len(res.Sentences) > MaxResultSentences {
			t.Fatal("too many sentences passed")
		}
	})
}

func FuzzParseEvent(f *testing.F) {
	for _, s := range []string{
		`{"type":"pong","seq":1}`, `{"type":"progress","job":"01A","fraction":0.5}`,
		`{"type":"fail","code":"busy"}`, `{"type":"hello","seq":1,"version":"3.1.0","platform":"linux-x64","jobs":""}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		ev, err := ParseEvent(line)
		if err != nil {
			if !errors.Is(err, ErrContract) {
				t.Fatalf("not a contract error: %v", err)
			}
			return
		}
		if ev.Type == "" || len(line) > MaxEventLine {
			t.Fatalf("accepted %q as %+v", line, ev)
		}
	})
}

func FuzzParseRequest(f *testing.F) {
	f.Add([]byte(`{"op":"ping","seq":1}`))
	f.Add([]byte(`{"op":"run","seq":4,"job":"01A","engine":"mlx","kit":"` + strings.Repeat("b", 64) + `","params":{"model":"m"}}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		r, err := ParseRequest(line)
		if err != nil {
			return
		}
		if r.Job != "" && !JobIDPattern.MatchString(r.Job) {
			t.Fatalf("a bad job id passed: %q", r.Job)
		}
		if r.Op == OpRun {
			m, err := CheckParams(r.Engine, r.Params)
			if err != nil {
				t.Fatal("run params passed ParseRequest but not CheckParams")
			}
			for _, k := range PathKeys[r.Engine] {
				if _, ok := m[k]; ok {
					t.Fatalf("a path key passed: %s", k)
				}
			}
		}
	})
}
