// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// workerTimings are the "timing" blocks the workers write, as the worker
// code builds them (no recorded result.json with every step is in the
// repository): worker/asr_common.py make_timing (load_s, transcribe_s,
// audio_s, first_compile, peak_working_set_mb, postprocess), its
// postprocess from worker/asr.py — "redecode": RedecodeStats.as_dict
// (windows, unpunctuated, uppercase, redecoded, accepted, redecode_s;
// "skipped": true with the switch off) — and worker/textrepair.py —
// "textrepair": repair_words' stats plus "ms", or {"version", "skipped":
// "language", "words", "ms"} for another language. Floats as Python's json
// writes them (3600.0).
var workerTimings = map[string]string{
	"faster-whisper, every step": `{"load_s": 4.21, "transcribe_s": 812.37, "audio_s": 3600.0, "first_compile": false,
		"peak_working_set_mb": 2683, "postprocess": {
		"redecode": {"windows": 120, "unpunctuated": 3, "uppercase": 1, "redecoded": 4, "accepted": 2, "redecode_s": 12.5},
		"textrepair": {"version": 1, "words": 21000, "segments": 2, "unpunctuated_segments": 1, "caps_segments": 1,
			"sentence_ends_added": 14, "commas_added": 9, "words_recased": 30, "train_sentences": 800, "ms": 412}}}`,
	"redecode off, another language": `{"load_s": 0.0, "transcribe_s": 0.6, "audio_s": 60.0, "first_compile": true,
		"peak_working_set_mb": 0, "postprocess": {
		"redecode": {"skipped": true, "windows": 2, "unpunctuated": 0, "uppercase": 0, "redecoded": 0, "accepted": 0, "redecode_s": 0.0},
		"textrepair": {"version": 1, "skipped": "language", "words": 120, "ms": 0}}}`,
	"no step": `{"load_s": 1.38, "transcribe_s": 0.6, "audio_s": 60.0, "first_compile": false, "peak_working_set_mb": 2683}`,
}

// decodeNumbers decodes JSON with its numbers as written.
func decodeNumbers(t *testing.T, s string) any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return v
}

// SEC41 L5: checkTiming limits the timing block (depth, string length, no
// arrays); this pins it to what the workers write: each of their timing
// blocks passes ValidateResult with every key and value as it was.
func TestValidateResultKeepsTheWorkersTiming(t *testing.T) {
	for name, timing := range workerTimings {
		raw := `{"prompt_terms_used": 3, "sentences": [{"start_ms": 0, "end_ms": 1000, "text": "Hello.", "low_conf_words": []}], "timing": ` + timing + `}`
		out, _, err := ValidateResult([]byte(raw), 3600000)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got struct {
			Timing json.RawMessage `json:"timing"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if a, b := decodeNumbers(t, timing), decodeNumbers(t, string(got.Timing)); !reflect.DeepEqual(a, b) {
			t.Errorf("%s: timing changed\n got %s\nwant %s", name, got.Timing, timing)
		}
	}
}

// SEC41 L4: without the audio's length the times of a result have no upper
// bound (99999999999 passed); a length of 0 or less is refused outright —
// the asr stage never sends such a lecture to a remote (it refuses an
// unreadable or empty audio.wav first). A real length checks as before.
func TestValidateResultNeedsTheAudioLength(t *testing.T) {
	raw := []byte(`{"sentences":[{"start_ms":0,"end_ms":99999999999,"text":"x"}]}`)
	for _, ms := range []int64{0, -1, -60000} {
		if _, _, err := ValidateResult(raw, ms); err == nil || !strings.Contains(err.Error(), "audio length") {
			t.Errorf("audio of %d ms: %v, want refused for the audio length", ms, err)
		}
	}
	ok := []byte(`{"sentences":[{"start_ms":0,"end_ms":1000,"text":"x"}]}`)
	if _, _, err := ValidateResult(ok, 1000); err != nil {
		t.Errorf("audio of 1000 ms: %v", err)
	}
}

// SEC41 L3: ZWNJ (U+200C) and ZWJ (U+200D) are part of the text in Persian,
// Hindi and other scripts and in emoji sequences; the text of a remote
// result's sentences and words keeps them (and U+FE0F), so a Colab or SSH
// transcript reads like a local one. The bidi controls, U+200B, U+2060 and
// the BOM are still removed, and the short strings of the timing block
// (identifiers, CleanText) are cleaned as before.
func TestValidateResultKeepsJoinersInText(t *testing.T) {
	r := func(cs ...rune) string { return string(cs) }
	var (
		persian = r(0x0645, 0x06cc, 0x200c, 0x062e, 0x0648, 0x0627, 0x0647, 0x0645) // "I want", with ZWNJ
		hindi   = r(0x0915, 0x094d, 0x200d, 0x0937)                                 // a half form with ZWJ
		emoji   = r(0x1f469, 0x200d, 0x1f4bb, ' ', 0x2764, 0xfe0f)                  // a ZWJ sequence, a variation selector
		// bidi and invisible format characters
		hostile = r('a', 0x202e, 'b', 0x200b, 'c', 0x2060, 'd', 0xfeff, 'e', 0x200e, 'f', 0x2066, 'g')
	)
	text := persian + " " + hindi + " " + emoji + " " + hostile
	raw, err := json.Marshal(map[string]any{
		"sentences": []any{map[string]any{
			"start_ms": 0, "end_ms": 1000, "text": text,
			"low_conf_words": []any{map[string]any{"i": 0, "w": persian}},
			"words":          []any{map[string]any{"w": hindi, "s": 0, "e": 500}, map[string]any{"w": emoji, "s": 500, "e": 1000}},
		}},
		"timing": map[string]any{"note": r('x', 0x200d, 'y', 0x202e, 'z')},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, res, err := ValidateResult(raw, 3000)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Sentences[0]
	if want := persian + " " + hindi + " " + emoji + " abcdefg"; s.Text != want {
		t.Errorf("text %+q, want %+q", s.Text, want)
	}
	if s.LowConfWords[0].W != persian || s.Words[0].W != hindi || s.Words[1].W != emoji {
		t.Errorf("words %+q %+q %+q", s.LowConfWords[0].W, s.Words[0].W, s.Words[1].W)
	}
	if got := res.Timing["note"]; got != "xyz" {
		t.Errorf("timing string %+q, want the joiner removed there", got)
	}
	for _, r := range []rune{0x202e, 0x200b, 0x2060, 0xfeff, 0x200e, 0x2066} {
		if strings.ContainsRune(s.Text, r) {
			t.Errorf("text keeps U+%04X", r)
		}
	}
}
