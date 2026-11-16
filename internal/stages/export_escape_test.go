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

// Security review 2, L3: model output is escaped in the exports. VTT cue
// text by the WebVTT rules ("&", "<", and the ">" of "-->"); bilingual.md
// without raw HTML or image syntax, everything else as written.

func TestCueTextEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                "plain text",
		"Tom & Jerry":               "Tom &amp; Jerry",
		"x < y and y > z":           "x &lt; y and y > z",
		"<b>bold</b>":               "&lt;b>bold&lt;/b>",
		"<c.yellow>styled</c>":      "&lt;c.yellow>styled&lt;/c>",
		"A --> B":                   "A --&gt; B",
		"A ---> B":                  "A ---&gt; B",
		"<-->":                      "&lt;--&gt;",
		"&lt; is already an entity": "&amp;lt; is already an entity",
		"two\nlines  and\ttabs":     "two lines and tabs",
		"中文 <i>斜体</i> & 符号 -->":     "中文 &lt;i>斜体&lt;/i> &amp; 符号 --&gt;",
		"":                          "",
	} {
		if got := cueText(in); got != want {
			t.Errorf("cueText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMDTextEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                                 "plain text",
		"a < b, x <= y, i < 10, <- arrow, 3<4":       "a < b, x <= y, i < 10, <- arrow, 3<4",
		"Tom & Jerry &amp; co":                       "Tom & Jerry &amp; co",
		"x<y":                                        "x&lt;y",
		`<img src="https://evil.example/t.png">`:     `&lt;img src="https://evil.example/t.png">`,
		"<IMG SRC=x onerror=y>":                      "&lt;IMG SRC=x onerror=y>",
		"</script> <!-- c --> <?php ?> <!DOCTYPE x>": "&lt;/script> &lt;!-- c --> &lt;?php ?> &lt;!DOCTYPE x>",
		"![tracker](https://evil.example/t.png)":     `!\[tracker]\(https://evil.example/t.png)`,
		"![a][ref] and !![b](u)":                     `!\[a][ref] and !!\[b]\(u)`,
		"[a link](https://example.com) is inert":     `[a link]\(https://example.com) is inert`,
		"Wow! [note]":                                "Wow! [note]",
		"<https://example.com> autolink":             "&lt;https://example.com> autolink",
		"中文<b>粗体</b>！[注]":                            "中文&lt;b>粗体&lt;/b>！[注]",
		"trailing <":                                 "trailing <",
		"trailing !":                                 "trailing !",
	} {
		if got := mdText(in); got != want {
			t.Errorf("mdText(%q) = %q, want %q", in, got, want)
		}
	}
}

// rawHTMLRe finds what a Markdown renderer could take for raw HTML: "<"
// followed by a letter, "/", "!" or "?".
var rawHTMLRe = regexp.MustCompile(`<[A-Za-z/!?]`)

// The export stage writes the escaped texts: bilingual.md keeps the SPEC
// §6.6 layout and has no raw HTML or image syntax; every VTT cue text line
// has no "<", no "-->" and only escaped "&".
func TestExportEscapesModelOutput(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("Escapes", "m.mp4")
	texts := []struct{ en, zh string }{
		{`Look at <img src="https://evil.example/t.png"> this.`, `看这个 <img src=x onerror=alert(1)>。`},
		{"![pixel](https://evil.example/p.png) and Tom & Jerry.", "![像素](https://evil.example/p.png) 和 Tom & Jerry。"},
		{"If i < 10 and x<y then A --> B.", "如果 i < 10 且 x<y，则 A --> B。"},
		{"Close </script><!-- note -->.", "<b>粗体</b>"},
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
	r := e.run(e.stage(e.deps(nil), model.StageExport), l.ID)
	if r.err != nil {
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
	if m := rawHTMLRe.FindString(md); m != "" || strings.Contains(md, "![") {
		t.Errorf("bilingual.md has raw HTML (%q) or an image:\n%s", m, md)
	}
	for _, want := range []string{
		"**[00:00]** Look at &lt;img src=\"https://evil.example/t.png\"> this.\n\n看这个 &lt;img src=x onerror=alert(1)>。\n\n",
		"**[00:04]** !\\[pixel]\\(https://evil.example/p.png) and Tom & Jerry.\n\n!\\[像素]\\(https://evil.example/p.png) 和 Tom & Jerry。\n\n",
		"**[00:08]** If i < 10 and x&lt;y then A --> B.\n\n如果 i < 10 且 x&lt;y，则 A --> B。\n\n",
		"**[00:12]** Close &lt;/script>&lt;!-- note -->.\n\n&lt;b>粗体&lt;/b>\n\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("bilingual.md lacks %q:\n%s", want, md)
		}
	}
	// en.txt is plain text: as written
	if en := read(".en.txt"); !strings.Contains(en, `Look at <img src="https://evil.example/t.png"> this.`) {
		t.Errorf("en.txt changed the text:\n%s", en)
	}

	entity := regexp.MustCompile(`&(amp|lt|gt);`)
	for _, suffix := range []string{".zh.vtt", ".bilingual.vtt"} {
		vtt := read(suffix)
		if !strings.HasPrefix(vtt, "WEBVTT\n\n") {
			t.Fatalf("%s header:\n%s", suffix, vtt)
		}
		for _, block := range strings.Split(strings.TrimSpace(strings.TrimPrefix(vtt, "WEBVTT\n\n")), "\n\n") {
			lines := strings.Split(block, "\n")
			if len(lines) < 3 || !strings.Contains(lines[1], " --> ") {
				t.Fatalf("%s: malformed cue %q", suffix, block)
			}
			for _, text := range lines[2:] {
				if strings.Contains(text, "<") || strings.Contains(text, "-->") || strings.Contains(entity.ReplaceAllString(text, ""), "&") {
					t.Errorf("%s: unescaped cue text %q", suffix, text)
				}
			}
		}
		if !strings.Contains(vtt, "&lt;img src=x onerror=alert(1)>") || !strings.Contains(vtt, "A --&gt; B") || !strings.Contains(vtt, "Tom &amp; Jerry") {
			t.Errorf("%s: escaped texts missing:\n%s", suffix, vtt)
		}
	}
}
