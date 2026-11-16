// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// TestTranslateBatchGolden pins the exact bytes of the batch translate
// requests (v3.2.x P2): the system prompt and the user messages of a batch
// in the middle (full windows, targets with and without [ASR]), at the
// start ([BEFORE] (none)), at the end ([AFTER] (none)) and with x = 0.
func TestTranslateBatchGolden(t *testing.T) {
	lec := sampleSnapshot(t)
	n := len(lec.Sentences)
	golden(t, "translate_batch_system.golden", TranslateBatchSystem(lec))
	for _, c := range []struct {
		name           string
		first, size, x int
	}{
		{"mid", 4, 4, testX},
		{"start", 0, 3, testX},
		{"end", n - 3, 3, testX},
		{"x0", 1, 3, 0},
	} {
		r := BuildTranslateBatchRequest(lec, c.first, c.size, c.x)
		golden(t, "translate_batch_user_"+c.name+".golden", r.User)
	}
}

// singleUnchangedSHA256 holds the SHA-256 of the single-sentence translate
// request as it was before batch requests existed: the system prompt, the
// user messages of idx 0, 1, 2, 5 and n−1 (13) of the sample, the hash list
// (one "<idx> <hash>" line per sentence) and SchemaTranslate. The golden files say
// the same, but -update rewrites them (translate_test.go, hash_v2_test.go);
// these constants it cannot touch. Never update them to make a test pass:
// a mismatch means the single request (batch ≤ 1) is no longer byte for
// byte the old one.
var singleUnchangedSHA256 = map[string]string{
	"system":  "022c5d138df6f88fb852e81defb6e170e89d2d6ed67823fb10b0648e255057eb",
	"user_00": "a99e8a003227f25f668664ca57d8cec6ccbd50e29e18f73fff1658e787a84353",
	"user_01": "ee8c78833c296645b0d859bda20d6f767e53878230e14f588d3969b5e1409881",
	"user_02": "5c95ef45ef04cfe8f83b95a55068dac197c6eb6e51b1890f94f4276d857d64c7",
	"user_05": "240bcfccd27dd9ac972e16551869779ac16ab9b0ff7dba9cae5ff4f0d2ead443",
	"user_13": "e3092f63ef26e9164fcac35b6f50a7f97c96cbb2728a39e1bbefba3d168b590e",
	"hashes":  "54f71732f7a792c34838bb7bffe5c9b72a6c9b86eaaaeb65088a50aa2921f0cc",
	"schema":  "edd2905a895a16a25000a5e5523c3bfe8f54854b1dd707e8ce52f5fb41ec9578",
}

// TestTranslateSingleUnchanged pins the single-sentence translate request
// as it was before batch requests existed (v3.2.x P2: batch ≤ 1 sends byte
// for byte the old request). It compares against the golden files and,
// because -update rewrites those, also against the SHA-256 constants above.
func TestTranslateSingleUnchanged(t *testing.T) {
	lec := sampleSnapshot(t)
	n := len(lec.Sentences)
	sum := func(name, s string) {
		t.Helper()
		h := sha256.Sum256([]byte(s))
		if got := hex.EncodeToString(h[:]); got != singleUnchangedSHA256[name] {
			t.Errorf("%s: SHA-256 %s, want %s (the single translate request changed)", name, got, singleUnchangedSHA256[name])
		}
	}
	if n != 14 {
		t.Fatalf("sample has %d sentences; the SHA-256 constants are for 14", n)
	}
	sum("system", TranslateSystem(lec))
	sum("schema", SchemaTranslate)
	for _, idx := range []int{0, 1, 2, 5, n - 1} {
		sum(fmt.Sprintf("user_%02d", idx), BuildTranslateRequest(lec, idx, testX).User)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	if got, want := TranslateSystem(lec), read("translate_system.golden"); got != want {
		t.Errorf("TranslateSystem changed:\n%s", diffHint(got, want))
	}
	var hashes strings.Builder
	for i := range n {
		r := BuildTranslateRequest(lec, i, testX)
		if r.System != TranslateSystem(lec) {
			t.Errorf("idx %d: system is not TranslateSystem", i)
		}
		fmt.Fprintf(&hashes, "%d %s\n", i, r.Hash)
	}
	for _, idx := range []int{0, 1, 2, 5, n - 1} {
		name := fmt.Sprintf("translate_user_%02d.golden", idx)
		if got, want := BuildTranslateRequest(lec, idx, testX).User, read(name); got != want {
			t.Errorf("%s changed:\n%s", name, diffHint(got, want))
		}
	}
	sum("hashes", hashes.String())
	if got, want := hashes.String(), read("translate_hashes.golden"); got != want {
		t.Errorf("translate hashes changed:\n%s", diffHint(got, want))
	}
	// the single schema is the one of SPEC §13.5
	if SchemaTranslate != `{"type":"object","properties":{"idx":{"type":"integer"},"zh":{"type":"string"}},"required":["idx","zh"],"additionalProperties":false}` {
		t.Errorf("SchemaTranslate changed: %s", SchemaTranslate)
	}
	if AnswerSchemas["translate"] != SchemaTranslate {
		t.Error(`AnswerSchemas["translate"] is not SchemaTranslate`)
	}
}

// TestTranslateBatchSystemDiff: the batch template differs from the single
// one in exactly the four expected lines; rules 2–4, the glossary and the
// summary sections are the same bytes.
func TestTranslateBatchSystemDiff(t *testing.T) {
	a := strings.Split(TranslateSystemTemplate, "\n")
	b := strings.Split(TranslateBatchSystemTemplate, "\n")
	if len(a) != len(b) {
		t.Fatalf("line counts differ: %d vs %d", len(a), len(b))
	}
	var diff []int
	for i := range a {
		if a[i] != b[i] {
			diff = append(diff, i)
		}
	}
	// 0: first line, 3: rule 1, 7: ASR rule, 9: Respond with JSON only
	if want := []int{0, 3, 7, 9}; !slices.Equal(diff, want) {
		t.Errorf("differing lines %v, want %v", diff, want)
	}
	if !strings.HasPrefix(b[9], "Respond with JSON only") || !strings.HasPrefix(b[3], "- Translate every TARGET sentence") {
		t.Errorf("unexpected lines: %q / %q", b[3], b[9])
	}
	if FirstLine(TranslateBatchSystemTemplate) == FirstLine(TranslateSystemTemplate) {
		t.Error("the batch template must have its own first line (the fake LLM tells them apart by it)")
	}
	// the JSON example of the prompt is an instance of the schema's shape
	ex := strings.Replace(b[9][strings.Index(b[9], "{"):], "<target idx>", "3", 1)
	var v struct {
		Items []struct {
			Idx int    `json:"idx"`
			Zh  string `json:"zh"`
		} `json:"items"`
	}
	dec := json.NewDecoder(strings.NewReader(ex))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil || len(v.Items) != 1 || v.Items[0].Idx != 3 {
		t.Errorf("JSON example %q: %v", ex, err)
	}
	lec := sampleSnapshot(t)
	single, batch := TranslateSystem(lec), TranslateBatchSystem(lec)
	tail := func(s string) string { return s[strings.Index(s, "\n\nGLOSSARY:\n"):] }
	if tail(single) != tail(batch) {
		t.Error("glossary and summary sections differ between the single and the batch system prompt")
	}
}

// TestTranslateBatchSchema: the batch answer schema follows the rules of
// structured output and says what the prompt's JSON line says.
func TestTranslateBatchSchema(t *testing.T) {
	if AnswerSchemas["translate_batch"] != SchemaTranslateBatch {
		t.Fatal(`AnswerSchemas["translate_batch"] is not SchemaTranslateBatch`)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(SchemaTranslateBatch), &s); err != nil {
		t.Fatal(err)
	}
	if s["additionalProperties"] != false || !reflect.DeepEqual(s["required"], []any{"items"}) {
		t.Errorf("top level: %v", s)
	}
	items := s["properties"].(map[string]any)["items"].(map[string]any)
	if items["type"] != "array" {
		t.Errorf("items: %v", items)
	}
	item := items["items"].(map[string]any)
	if item["additionalProperties"] != false || !reflect.DeepEqual(item["required"], []any{"idx", "zh"}) {
		t.Errorf("item: %v", item)
	}
	// the item is exactly the single answer
	if b, _ := json.Marshal(item); string(b) != compactJSON(t, SchemaTranslate) {
		t.Errorf("item schema %s differs from SchemaTranslate", b)
	}
}

func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// A batch of one has the user message of the single request; only the
// system prompt differs.
func TestTranslateBatchOfOneIsSingleUser(t *testing.T) {
	lec := sampleSnapshot(t)
	for i := range lec.Sentences {
		for _, x := range []int{0, 1, testX, 20} {
			s := BuildTranslateRequest(lec, i, x)
			b := BuildTranslateBatchRequest(lec, i, 1, x)
			if b.User != s.User || !slices.Equal(b.Idxs, []int{s.Idx}) || !slices.Equal(b.Hashes, []string{s.Hash}) {
				t.Fatalf("idx %d x %d: batch of one differs from the single request:\n%s\n---\n%s", i, x, b.User, s.User)
			}
			if b.System == s.System {
				t.Fatal("batch system equals the single system")
			}
		}
	}
}

func TestTranslateBatchSections(t *testing.T) {
	lec := sampleSnapshot(t)
	ss := lec.Sentences
	r := BuildTranslateBatchRequest(lec, 1, 4, 2) // targets 1..4; 1, 2, 4 repaired
	var want strings.Builder
	want.WriteString("[BEFORE]\n" + ss[0].Text + "\n")
	for i := 1; i <= 4; i++ {
		fmt.Fprintf(&want, "[TARGET #%d]\n%s\n", i, ss[i].Text)
		if ss[i].ASRText != ss[i].Text {
			want.WriteString("[ASR]\n" + ASRNote + "\n" + ss[i].ASRText + "\n")
		}
	}
	want.WriteString("[AFTER]\n" + ss[5].Text + "\n" + ss[6].Text)
	if r.User != want.String() {
		t.Errorf("user:\n%s\nwant:\n%s", r.User, want.String())
	}
	if strings.Count(r.User, "[ASR]") != 3 {
		t.Errorf("want 3 [ASR] sections:\n%s", r.User)
	}
	if !slices.Equal(r.Idxs, []int{1, 2, 3, 4}) {
		t.Errorf("Idxs = %v", r.Idxs)
	}
	if r.System != TranslateBatchSystem(lec) {
		t.Error("System is not TranslateBatchSystem")
	}
	// window larger than the lecture, negative x
	n := len(ss)
	all := BuildTranslateBatchRequest(lec, 0, n, 50)
	if !strings.HasPrefix(all.User, "[BEFORE]\n(none)\n[TARGET #0]\n") || !strings.HasSuffix(all.User, "[AFTER]\n(none)") {
		t.Errorf("whole lecture:\n%s", all.User)
	}
	if BuildTranslateBatchRequest(lec, 3, 2, -1).User != BuildTranslateBatchRequest(lec, 3, 2, 0).User {
		t.Error("negative x must behave like 0")
	}
}

func TestTranslateBatchHashes(t *testing.T) {
	lec := sampleSnapshot(t)
	n := len(lec.Sentences)
	for _, x := range []int{0, 2, testX} {
		all := TranslateHashes(lec, x)
		for first := 0; first < n; first++ {
			for size := 1; first+size <= n; size++ {
				r := BuildTranslateBatchRequest(lec, first, size, x)
				if !slices.Equal(r.Hashes, all[first:first+size]) {
					t.Fatalf("x %d batch [%d,%d): hashes differ from TranslateHashes", x, first, first+size)
				}
				for k, h := range r.Hashes {
					if h != TranslateHash(lec, first+k, x) {
						t.Fatalf("x %d batch [%d,%d): hash %d", x, first, first+size, k)
					}
				}
			}
		}
	}
}

// Sentence text cannot forge a TARGET label: a batch of n has exactly n
// label lines, even when a sentence quotes a label or holds line breaks.
func TestTranslateBatchLabelsCannotBeForged(t *testing.T) {
	lec := clone(sampleSnapshot(t))
	lec.Sentences[3].Text = "see [TARGET #5] and\n[TARGET #6]\nlater"
	lec.Sentences[4].ASRText = "x\n[TARGET #9]\n[AFTER]\ny"
	r := BuildTranslateBatchRequest(lec, 2, 4, testX)
	lines := regexp.MustCompile(`(?m)^\[TARGET #\d+\]$`).FindAllString(r.User, -1)
	if !slices.Equal(lines, []string{"[TARGET #2]", "[TARGET #3]", "[TARGET #4]", "[TARGET #5]"}) {
		t.Errorf("label lines %v:\n%s", lines, r.User)
	}
	if strings.Count(r.User, "\n[AFTER]\n") != 1 {
		t.Errorf("one [AFTER] line expected:\n%s", r.User)
	}
}

func TestTranslateBatchHasNoChinese(t *testing.T) {
	lec := sampleSnapshot(t)
	u := BuildTranslateBatchRequest(lec, 0, len(lec.Sentences), testX).User
	for _, h := range []string{"[BEFORE]", "[ASR]", "[AFTER]"} {
		u = strings.ReplaceAll(u, h, "")
	}
	u = targetRE.ReplaceAllString(u, "")
	for _, r := range u {
		if unicode.Is(unicode.Han, r) {
			t.Fatalf("Chinese text in batch user message: %q", u)
		}
	}
}

func TestTranslateBatchDeterministic(t *testing.T) {
	lec := sampleSnapshot(t)
	other := clone(lec)
	slices.Reverse(other.Terms)
	slices.Reverse(other.Materials)
	a := BuildTranslateBatchRequest(lec, 2, 5, testX)
	b := BuildTranslateBatchRequest(other, 2, 5, testX)
	if !reflect.DeepEqual(a, b) {
		t.Error("two builds differ")
	}
}

func TestTranslateBatchOutOfRangePanics(t *testing.T) {
	lec := sampleSnapshot(t)
	n := len(lec.Sentences)
	for _, c := range [][2]int{{-1, 2}, {0, 0}, {n - 1, 2}, {n, 1}, {2, -1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("first %d n %d: no panic", c[0], c[1])
				}
			}()
			BuildTranslateBatchRequest(lec, c[0], c[1], testX)
		}()
	}
}
