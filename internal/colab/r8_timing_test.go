// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// SEC41 L5: the timing block the Colab worker writes, as worker/colab_remote.py
// builds it (no recorded result.json is in the repository): make_timing's
// fields (worker/asr_common.py), the postprocess counters of the parts
// merged (redecode, textrepair: worker/asr.py, worker/textrepair.py), and
// for 极速模式 "split": {"parts", "cuts_ms"} (transcribe_split). checkResult
// keeps every key and value — cuts_ms, an array the shared check drops, is
// put back.
func TestColabResultKeepsTheWorkersTiming(t *testing.T) {
	const timing = `{"load_s": 38.4, "transcribe_s": 401.27, "audio_s": 3600.0, "first_compile": false,
		"peak_working_set_mb": 5120, "postprocess": {
		"redecode": {"windows": 240, "unpunctuated": 5, "uppercase": 2, "redecoded": 7, "accepted": 3, "redecode_s": 21.75},
		"textrepair": {"version": 1, "words": 21000, "segments": 3, "unpunctuated_segments": 2, "caps_segments": 1,
			"sentence_ends_added": 20, "commas_added": 11, "words_recased": 40, "train_sentences": 900, "ms": 655},
		"split": {"parts": 3, "cuts_ms": [1198000, 2401500]}}}`
	raw := `{"prompt_terms_used": 2, "sentences": [{"start_ms": 0, "end_ms": 1000, "text": "Hello.", "low_conf_words": []}], "timing": ` + timing + `}`
	out, err := checkResult([]byte(raw), 3600000)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Timing json.RawMessage `json:"timing"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	decode := func(s string) any {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("%v: %s", err, s)
		}
		return v
	}
	if a, b := decode(timing), decode(string(got.Timing)); !reflect.DeepEqual(a, b) {
		t.Errorf("timing changed\n got %s\nwant %s", got.Timing, timing)
	}
}
