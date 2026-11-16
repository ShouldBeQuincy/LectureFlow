// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// AUD5 low-2: bilingual.md has no Markdown link from model output (or a
// human edit). An inline link needs "(" right after the "]" of its text and
// a link reference definition needs ":" right after its label (CommonMark;
// a definition may also stand in a block quote or list item); a backslash
// between them leaves the text as written on screen but no link. Without
// definitions, reference links ("[a][b]", "[a]") stay text too, and so do
// the "**[mm:ss]**" timestamps.

// linkSyntaxRe finds what can still make a link: a "]" followed directly by
// "(" or ":".
var linkSyntaxRe = regexp.MustCompile(`\][(:]`)

func TestMDTextNeutralizesLinks(t *testing.T) {
	for in, want := range map[string]string{
		"[点此查看讲义](https://example.invalid/x?d=1)": `[点此查看讲义]\(https://example.invalid/x?d=1)`,
		"[x](javascript:void(0))":                 `[x]\(javascript:void(0))`,
		`[ref]: https://example.invalid/ "t"`:     `[ref]\: https://example.invalid/ "t"`,
		"> [r]: https://example.invalid/":         `> [r]\: https://example.invalid/`,
		"- [00:04]: https://example.invalid/":     `- [00:04]\: https://example.invalid/`,
		"[^1]: a footnote":                        `[^1]\: a footnote`,
		"[a](<https://example.invalid/>)":         `[a]\(&lt;https://example.invalid/>)`,
		"[a [b] c](u) and [d](e)":                 `[a [b] c]\(u) and [d]\(e)`,
		`[a\](u)`:                                 `[a\]\(u)`,
		"见[讲义](https://example.invalid/)。[注]: 这里": `见[讲义]\(https://example.invalid/)。[注]\: 这里`,
	} {
		got := mdText(in)
		if got != want {
			t.Errorf("mdText(%q) = %q, want %q", in, got, want)
		}
		if m := linkSyntaxRe.FindString(got); m != "" {
			t.Errorf("mdText(%q) = %q still has %q", in, got, m)
		}
	}
}

// Brackets in ordinary text are written as they are: only a "]" directly
// followed by "(" or ":" gets a backslash, which a Markdown viewer does not
// show, so the text reads the same.
func TestMDTextKeepsPlainBrackets(t *testing.T) {
	for _, in := range []string{
		"Wow! [note]",
		"[注] 这个结论",
		"a[i] = b[j] + c[k]",
		"f(x) [1] (see above)",
		"[a] (b) and [c] : d",
		"[a][b] and [c][]",
		"time [00:04] is",
		"x ] ( y ] : z",
		"数组 arr[0] 是首元素（下标从 0 开始）",
	} {
		if got := mdText(in); got != in {
			t.Errorf("mdText(%q) = %q, want it unchanged", in, got)
		}
	}
	// read back without the added backslashes, the text is the original
	unescape := strings.NewReplacer(`]\(`, "](", `]\:`, "]:")
	for _, in := range []string{"[a](b)", "[r]: u", "x](y)]:z", "中文[链接](地址)：[定义]: 地址"} {
		if got := unescape.Replace(mdText(in)); got != in {
			t.Errorf("mdText(%q) reads back as %q", in, got)
		}
	}
}

// The export stage: bilingual.md changes only on the lines with link
// syntax; en.txt, the VTT files and the citation JSON keep the text as
// before.
func TestExportNeutralizesLinks(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("Links", "m.mp4")
	texts := []struct{ en, zh string }{
		{"See [the notes](https://example.invalid/n?d=1) now.", "[00:04]: https://example.invalid/t"},
		{"Plain [brackets] stay.", "普通的[方括号]不变。"},
	}
	var ss []model.Sentence
	for i, tx := range texts {
		ss = append(ss, model.Sentence{LectureID: l.ID, Idx: i, StartMS: int64(i) * 4000, EndMS: int64(i)*4000 + 3000,
			ASRText: tx.en, RepairedText: tx.en, ZhStatus: model.ZhNone})
	}
	e.insertSentences(l.ID, ss)
	for i, tx := range texts {
		z := tx.zh
		e.must(e.st.SetTranslation(e.ctx, l.ID, i, db.SentenceTr{Zh: &z, ZhStatus: model.ZhOK}))
	}
	if r := e.run(e.stage(e.deps(nil), model.StageExport), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	dir := filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName)
	read := func(suffix string) string {
		t.Helper()
		for _, n := range e.exportNames(l) {
			if strings.HasSuffix(n, suffix) {
				b, err := os.ReadFile(filepath.Join(dir, n))
				if err != nil {
					t.Fatal(err)
				}
				return strings.ReplaceAll(string(b), "\r\n", "\n")
			}
		}
		t.Fatalf("no %s export", suffix)
		return ""
	}

	md := read(".bilingual.md")
	want := "**[00:00]** See [the notes]\\(https://example.invalid/n?d=1) now.\n\n" +
		"[00:04]\\: https://example.invalid/t\n\n" +
		"**[00:04]** Plain [brackets] stay.\n\n" +
		"普通的[方括号]不变。\n\n"
	if md != want {
		t.Errorf("bilingual.md:\n%s\nwant:\n%s", md, want)
	}
	if en := read(".en.txt"); en != texts[0].en+"\n"+texts[1].en+"\n" {
		t.Errorf("en.txt changed the text:\n%s", en)
	}
	for _, suffix := range []string{".zh.vtt", ".bilingual.vtt"} {
		if vtt := read(suffix); !strings.Contains(vtt, texts[0].zh) || strings.Contains(vtt, `\`) {
			t.Errorf("%s changed the text:\n%s", suffix, vtt)
		}
	}
	if vtt := read(".bilingual.vtt"); !strings.Contains(vtt, "[the notes](https://example.invalid/n?d=1)") {
		t.Errorf("bilingual.vtt changed the English:\n%s", vtt)
	}
	if js := read(".cite.json"); !strings.Contains(js, texts[0].en) || !strings.Contains(js, texts[0].zh) || strings.Contains(js, `\\(`) {
		t.Errorf("cite.json changed the text:\n%s", js)
	}
}
