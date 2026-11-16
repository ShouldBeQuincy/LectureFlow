// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"strings"
	"testing"
)

// specIndentedBlocks returns the fenced blocks of a SPEC-v2 section (fences
// may carry a language and an indent), with the common two-space list
// indent removed.
func specIndentedBlocks(section string) []string {
	var blocks []string
	lines := strings.Split(section, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			continue
		}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
		var buf []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			buf = append(buf, strings.TrimPrefix(lines[i], indent))
		}
		blocks = append(blocks, strings.Join(buf, "\n"))
	}
	return blocks
}

// specPrompt returns the block of the section that starts with prefix.
func specPrompt(t *testing.T, heading, prefix string) string {
	t.Helper()
	for _, b := range specIndentedBlocks(specFileSection(t, "v2.md", heading)) {
		if strings.HasPrefix(b, prefix) {
			return b
		}
	}
	t.Fatalf("SPEC-v2.md %s: no code block starting with %q", heading, prefix)
	return ""
}

// promptEdit is one approved change of a SPEC-v2 prompt: the exact text
// old (found once in the SPEC block) becomes new.
type promptEdit struct{ old, new string }

// kindPromptEdits are the changes of the P13.3.2 and P13.5 prompts for
// lecture kinds (user request 2026-10-23, lead freeze
// .claude/kinds-brief.md 1.3), applied in order, and nothing else: prompt
// version 2, then for the filename rule version 3 (the last
// FilenameRuleSystem edit, lead after the real DeepSeek check). An edit's
// old text is looked up in the SPEC-v2 block as changed by the edits
// before it.
var kindPromptEdits = map[string][]promptEdit{
	"FilenameRuleSystem": {
		{"(?P<title>…). month may capture", "(?P<title>…), (?P<kind>…). month may capture"},
		{"- Do not capture week or lecture numbers.\n",
			"- If a part of the name is a code for the type of class activity (for example L and T in \"CSC1002-L01-01 …\" and \"CSC1002-T01-02 …\", meaning lecture and tutorial), capture only that code as kind, and in kind_map map every code the group captures to the id of one of the LECTURE KINDS given. If the names carry no such code, or you cannot tell what a code means, omit the kind group and give an empty kind_map.\n" +
				"- Do not capture week numbers or lecture sequence numbers (in \"L01-01\" the kind is \"L\"; the digits are not captured).\n"},
		{`{"regex": "...", "explanation": "one short sentence"}`, `{"regex": "...", "kind_map": {"<code>": "<kind id>"}, "explanation": "one short sentence"}`},
		// version 3
		{"omit the kind group and give an empty kind_map.\n",
			"omit the kind group and give an empty kind_map.\n" +
				"- Write the kind group so that it accepts any such code (for example (?P<kind>[A-Z]+)), not only the codes in the examples, and let the digits around it vary, so that later files of other activity types or streams still match; a code missing from kind_map is simply left unmapped.\n"},
	},
	"TitleSystem": {
		{"Respond with JSON only: {\"title_en\": \"...\", \"title_zh\": \"...\", \"blurb_en\": \"...\", \"blurb_zh\": \"...\"}",
			"Kind: the id of the one entry of LECTURE KINDS that fits this recording best, judged from the summary: a lecture, a tutorial or problem class, a seminar, a recording from an earlier year, supplementary material, or one of the user-defined kinds. If unsure, answer \"lecture\".\n" +
				"Respond with JSON only: {\"title_en\": \"...\", \"title_zh\": \"...\", \"blurb_en\": \"...\", \"blurb_zh\": \"...\", \"kind\": \"...\"}"},
	},
}

// TestOrganizerPromptsVerbatim: the P7, P13.3.2 and P13.5 prompts are the
// SPEC-v2 code blocks byte for byte — P13.3.2 and P13.5 with exactly the
// approved lecture-kinds edits (kindPromptEdits) — with distinct first
// lines (the fake LLM identifies the requests by them).
func TestOrganizerPromptsVerbatim(t *testing.T) {
	cases := []struct {
		name, heading, prefix, got string
	}{
		{"ChaptersSystem", "## P7. ", "You divide a university lecture", ChaptersSystem},
		{"FilenameRuleSystem", "#### P13.3.2 ", "You write one regular expression", FilenameRuleSystem},
		{"TitleSystem", "### P13.5 ", "You write a title", TitleSystem},
	}
	firsts := map[string]string{}
	for _, c := range cases {
		want := specPrompt(t, c.heading, c.prefix)
		for _, e := range kindPromptEdits[c.name] {
			if n := strings.Count(want, e.old); n != 1 {
				t.Fatalf("%s: the edited text %q occurs %d times in SPEC-v2 %s (after the edits before it)", c.name, e.old, n, c.heading)
			}
			want = strings.Replace(want, e.old, e.new, 1)
		}
		if c.got != want {
			t.Errorf("%s differs from SPEC-v2 %s:\n%s", c.name, c.heading, diffHint(c.got, want))
		}
		fl := FirstLine(c.got)
		if other, ok := firsts[fl]; ok {
			t.Errorf("%s and %s share their first line", c.name, other)
		}
		firsts[fl] = c.name
	}
	for _, other := range []string{GlossarySeedSystem, MaterialTermsSystem, SummarySystemTemplate, RepairSystemTemplate,
		TranslateSystemTemplate, SyllabusFormatSystem, JanitorSystem} {
		if name, ok := firsts[FirstLine(other)]; ok {
			t.Errorf("%s shares its first line with an older prompt", name)
		}
	}
}

func chapterSentences(texts ...string) []Sentence {
	ss := make([]Sentence, len(texts))
	for i, s := range texts {
		ss[i] = Sentence{Idx: i, Text: s, ASRText: "asr " + s}
	}
	return ss
}

func TestChaptersRequest(t *testing.T) {
	ss := chapterSentences("Welcome back.", "Today:\nstream ciphers.", "An LFSR is linear.")
	req := ChaptersRequest(" The lecture covers stream ciphers. ", ss)
	if req.System != ChaptersSystem {
		t.Fatal("system is not the P7 prompt")
	}
	want := "SUMMARY:\nThe lecture covers stream ciphers.\n\nTRANSCRIPT:\n[0] Welcome back.\n[1] Today: stream ciphers.\n[2] An LFSR is linear."
	if req.User != want {
		t.Errorf("user:\n got %q\nwant %q", req.User, want)
	}
	if again := ChaptersRequest(" The lecture covers stream ciphers. ", ss); again != req {
		t.Error("two builds differ")
	}
	if r := ChaptersRequest("", ss); !strings.HasPrefix(r.User, "SUMMARY:\n(none)\n\n") {
		t.Errorf("empty summary: %q", r.User)
	}
}

// TestChaptersInputHash: the hash follows the summary and every
// sentence.text (P7), and nothing else (asr_text, idx-only changes).
func TestChaptersInputHash(t *testing.T) {
	base := chapterSentences("a b", "c d", "e f")
	h := ChaptersInputHash("sum", base)
	if h != ChaptersInputHash("sum", chapterSentences("a b", "c d", "e f")) {
		t.Fatal("same input, different hash")
	}
	if len(h) != 64 {
		t.Fatalf("not a hex SHA-256: %q", h)
	}
	if ChaptersInputHash("sum2", base) == h {
		t.Error("summary change not detected")
	}
	if ChaptersInputHash("sum", chapterSentences("a b", "c D", "e f")) == h {
		t.Error("sentence text change not detected")
	}
	if ChaptersInputHash("sum", chapterSentences("a b", "c d")) == h {
		t.Error("dropped sentence not detected")
	}
	if ChaptersInputHash("sum", chapterSentences("a", "b c d", "e f")) == h {
		t.Error("moved text between sentences not detected")
	}
	asr := chapterSentences("a b", "c d", "e f")
	asr[1].ASRText = "different"
	if ChaptersInputHash("sum", asr) != h {
		t.Error("asr_text must not be part of the hash")
	}
}

// TestChaptersInputHashPinned pins ChaptersInputHash to literal values
// (review R26 A-7). Every lecture stores this hash with its chapters; a
// change to the function makes the chapters of every existing lecture show
// "章节可能已过时" (and, after a summary rerun, regenerate). Change these
// values only on purpose. (SHA-256 of "8:chapters3:sum1:33:a b3:c d3:e f"
// and of "8:chapters0:1:0".)
func TestChaptersInputHashPinned(t *testing.T) {
	if h := ChaptersInputHash("sum", chapterSentences("a b", "c d", "e f")); h != "506c2cf8b2dffaa8b70d0a8ba26200d9b90f7fa5660ddeb9700137fab01cf028" {
		t.Errorf("ChaptersInputHash changed: %s", h)
	}
	if h := ChaptersInputHash("", nil); h != "e1148ed1acbe76502a80a162705872c1e770a4e5efffae2d573373c7928e559e" {
		t.Errorf("ChaptersInputHash of nothing changed: %s", h)
	}
}

func TestTitleRequest(t *testing.T) {
	full := TitleRequest(TitleInput{Summary: "S.", SyllabusEntry: "| Week | Topic |\n| 3 | Stream ciphers |", OriginalFilename: "CSC1003_2026-10-06.mp4"})
	if full.System != TitleSystem {
		t.Fatal("system is not the P13.5 prompt")
	}
	want := "SUMMARY:\nS.\n\nSYLLABUS ENTRY FOR THIS WEEK:\n| Week | Topic |\n| 3 | Stream ciphers |\n\nORIGINAL FILE NAME:\nCSC1003_2026-10-06.mp4"
	if full.User != want {
		t.Errorf("user:\n got %q\nwant %q", full.User, want)
	}
	bare := TitleRequest(TitleInput{Summary: "S."})
	if bare.User != "SUMMARY:\nS." {
		t.Errorf("without entry and file name: %q", bare.User)
	}
	if strings.Contains(bare.User, "SYLLABUS") {
		t.Error("an absent syllabus entry must be left out (the prompt says 'if ... is provided')")
	}
}

func TestFilenameRuleRequest(t *testing.T) {
	in := FilenameRuleInput{
		CourseCode: "CSC1003",
		TermStart:  "2026-09-28",
		Samples: []FilenameSample{
			{Filename: "CSC1003-L02-01 Tue Sep 29.mp4", UploadedAt: "2026-10-20 14:03", CreationTime: "2026-09-29 10:02"},
			{Filename: "clip_Oct07.mp4", UploadedAt: "2026-10-21 09:00"},
		},
	}
	req := FilenameRuleRequest(in)
	if req.System != FilenameRuleSystem {
		t.Fatal("system is not the P13.3.2 prompt")
	}
	for _, s := range []string{
		"COURSE CODE: CSC1003\n",
		"TERM START (Monday of teaching week 1): 2026-09-28\n",
		"FILES (2)",
		"may be a download or transcoding time, not necessarily the recording time",
		"\nCSC1003-L02-01 Tue Sep 29.mp4 | 2026-10-20 14:03 | 2026-09-29 10:02\n",
		"\nclip_Oct07.mp4 | 2026-10-21 09:00 | -",
	} {
		if !strings.Contains(req.User, s) {
			t.Errorf("user lacks %q:\n%s", s, req.User)
		}
	}
	if strings.HasSuffix(req.User, "\n") {
		t.Error("trailing newline")
	}
	none := FilenameRuleRequest(FilenameRuleInput{Samples: in.Samples[1:]})
	if !strings.Contains(none.User, "COURSE CODE: (none)\nTERM START (Monday of teaching week 1): (none)\n") {
		t.Errorf("missing code/term start: %q", none.User)
	}
}

func TestZhLenAndEnglishWords(t *testing.T) {
	for _, c := range []struct {
		s    string
		want int
	}{
		{"流密码与线性反馈移位寄存器", 13},
		{"IND-CPA 回顾", 3},
		{"RSA 与 ElGamal 加密", 5},
		{"定位缓存行：标记、索引与偏移", 14},
		{"", 0},
		{"第 2 讲", 3},
	} {
		if got := ZhLen(c.s); got != c.want {
			t.Errorf("ZhLen(%q) = %d, want %d", c.s, got, c.want)
		}
	}
	if n := EnglishWords("  Stream Ciphers and   LFSRs "); n != 4 {
		t.Errorf("EnglishWords = %d", n)
	}
}

func TestSyllabusWeekEntry(t *testing.T) {
	table := `# CSC1003 Cryptography

## Course Information

Lecturer: someone

## Weekly Topics

| Week | Topic | Notes |
| ---: | --- | --- |
| 1 | Historical ciphers | |
| 2 | Stream ciphers | LFSR |
| 3–4 | Block ciphers | AES, modes |
| Week 5 | Hash functions | |
| W6 | MACs | |
| 7 | Reading week | |

## Assessment

| Week 2 | Coursework 1 |
`
	cases := []struct {
		week int
		want string
	}{
		{1, "| Week | Topic | Notes |\n| 1 | Historical ciphers | |"},
		{2, "| Week | Topic | Notes |\n| 2 | Stream ciphers | LFSR |"},
		{3, "| Week | Topic | Notes |\n| 3–4 | Block ciphers | AES, modes |"},
		{4, "| Week | Topic | Notes |\n| 3–4 | Block ciphers | AES, modes |"},
		{5, "| Week | Topic | Notes |\n| Week 5 | Hash functions | |"},
		{6, "| Week | Topic | Notes |\n| W6 | MACs | |"},
		{8, ""},
		{0, ""},
	}
	for _, c := range cases {
		if got := SyllabusWeekEntry(table, c.week); got != c.want {
			t.Errorf("table week %d:\n got %q\nwant %q", c.week, got, c.want)
		}
	}

	// the week column is found by its header, not by position
	dated := "## Weekly Topics\n\n| Date | Week | Topic |\n|---|---|---|\n| 3 Oct | 1 | Intro |\n| 10 Oct | 2 | Caches |\n"
	if got := SyllabusWeekEntry(dated, 3); got != "" {
		t.Errorf("date column taken as week: %q", got)
	}
	if got := SyllabusWeekEntry(dated, 2); got != "| Date | Week | Topic |\n| 10 Oct | 2 | Caches |" {
		t.Errorf("week column: %q", got)
	}
	// a table without a week column is not used
	if got := SyllabusWeekEntry("## Weekly Topics\n\n| # | Topic |\n|---|---|\n| 1 | Intro |\n", 1); got != "" {
		t.Errorf("table without a week column: %q", got)
	}

	list := "## Weekly Topics\n\n- Week 1: Introduction\n- **Week 2** – Stream ciphers\n- Weeks 3-4: Block ciphers\n- Week 10: Revision\n\n## Key Terms\n\nWeek 1 terms\n"
	for week, want := range map[int]string{
		1:  "- Week 1: Introduction",
		2:  "- **Week 2** – Stream ciphers",
		4:  "- Weeks 3-4: Block ciphers",
		10: "- Week 10: Revision",
		5:  "",
	} {
		if got := SyllabusWeekEntry(list, week); got != want {
			t.Errorf("list week %d: got %q want %q", week, got, want)
		}
	}

	// A8.3: a list without week numbers means the weeks are not
	// identifiable; never guess by position (the real CSC1003 syllabus).
	plain := "## Weekly Topics\n\n- Historical overview of cryptography.\n- Private-key cryptography.\n- Public-key cryptography.\n"
	if got := SyllabusWeekEntry(plain, 2); got != "" {
		t.Errorf("unnumbered list: %q", got)
	}

	sub := "## Weekly Topics\n\n### Week 1\n- Caches\n- Write-back\n\n### Week 2\n- Virtual memory\n\n## Assessment\n- Exam\n"
	if got := SyllabusWeekEntry(sub, 1); got != "### Week 1\n- Caches\n- Write-back" {
		t.Errorf("sub-heading week 1: %q", got)
	}
	if got := SyllabusWeekEntry(sub, 2); got != "### Week 2\n- Virtual memory" {
		t.Errorf("sub-heading week 2: %q", got)
	}

	if got := SyllabusWeekEntry("# Course\n\nNo topics here.\n| Week | Topic |\n| 1 | x |\n", 1); got != "" {
		t.Errorf("no Weekly Topics section: %q", got)
	}
	long := "## Weekly Topics\n\n- Week 1: " + strings.Repeat("x", 3000) + "\n"
	if got := SyllabusWeekEntry(long, 1); len([]rune(got)) != SyllabusEntryMaxChars {
		t.Errorf("long entry: %d chars", len([]rune(got)))
	}
	// CRLF syllabus
	if got := SyllabusWeekEntry(strings.ReplaceAll(list, "\n", "\r\n"), 1); got != "- Week 1: Introduction" {
		t.Errorf("CRLF: %q", got)
	}
}
