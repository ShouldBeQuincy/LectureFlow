// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package qa

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

var glossary = []model.Term{
	{En: "cache", EnKey: "cache", Zh: "缓存", Enabled: true},
	{En: "cache line", EnKey: "cache line", Zh: "缓存行", Enabled: true},
	{En: "write-back", EnKey: "write-back", Zh: "写回", Enabled: true},
	{En: "LRU", EnKey: "lru", Zh: "", Enabled: true},
	{En: "TCP", EnKey: "tcp", Zh: "", Enabled: true},
	{En: "UDP", EnKey: "udp", Zh: "", Enabled: true},
	{En: "TLB", EnKey: "tlb", Zh: "快表", Enabled: false},
	{En: "page table", EnKey: "page table", Zh: "页 表", Enabled: true}, // spaces in zh are ignored
	{En: "bus", EnKey: "bus", Zh: "总线", Enabled: true},
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name, target, zh string
		ok               bool
		reason           string // substring of the reason when !ok
	}{
		// rule 1: non-empty
		{"empty", "Hello there.", "", false, "the translation is empty"},
		{"blank", "Hello there.", "  \n ", false, "the translation is empty"},

		// rule 2: glossary
		{"term used", "A write-back cache is fast.", "写回缓存很快。", true, ""},
		{"term missing", "A write-back cache is fast.", "回写式高速存储很快。", false, `glossary term "write-back" must be translated as "写回"`},
		{"term case-insensitive in target", "The CACHE is small.", "缓存很小。", true, ""},
		// SPEC-v2 G5.4: "cache" only occurs inside "cache line", so only the longer term is checked
		{"term inside a longer term", "Each cache line is small.", "每一行都很小。", false, `glossary term "cache line" must be translated as "缓存行"`},
		{"two terms missing", "The cache and each cache line.", "存储和每一行。", false, `"cache" must be translated as "缓存"; glossary term "cache line"`},
		{"not a whole word", "The data is cached here.", "数据在这里被暂存。", true, ""},
		{"plural s requires the term", "Both caches are full.", "两个高速存储都满了。", false, `glossary term "cache" must be translated as "缓存"`},
		{"plural s satisfied", "Both caches are full.", "两个缓存都满了。", true, ""},
		{"plural s of a multi-word term", "Walk the page tables.", "遍历页表。", true, ""},
		{"plural s of a multi-word term missing", "Walk the page tables now.", "遍历各个表。", false, `"page table"`},
		{"plural only on the last word", "The pages table.", "那些页的表格。", true, ""},
		{"no substring match", "The data is cacheable.", "数据可以被暂存。", true, ""},
		{"plural es satisfied", "Two buses connect them.", "两条总线把它们连起来。", true, ""},
		{"plural es requires the term", "Two buses connect them.", "两条线路把它们连起来。", false, `glossary term "bus" must be translated as "总线"`},
		{"disabled term ignored", "The TLB is a cache of the page table.", "TLB 是页表的缓存。", true, ""},
		{"zh with spaces matches", "Walk the page table.", "遍历页表。", true, ""},
		{"term without zh not required", "LRU is simple.", "最近最少使用很简单。", true, ""},

		// rule 3: English residue
		{"mostly English", "The cache is warm now.", "The 缓存 is warm now.", false, "mostly English"},
		{"untranslated copy", "Okay, so this is the plan for today.", "Okay, so this is the plan for today.", false, "mostly English"},
		{"English-only terms not residue", "TCP and UDP.", "TCP 和 UDP。", true, ""},
		{"acronym short sentence", "LRU replacement.", "LRU 替换。", true, ""},
		{"identifier kept but mostly Chinese", "We call malloc to get memory from the heap.", "我们调用 malloc 从堆上获取内存。", true, ""},
		{"code target allows Latin", "x = (a + b) * c;", "x = (a + b) * c;", true, ""},
		{"code-ish below threshold still fails", "Call foo(bar) to start the server now.", "Call foo(bar) to start the server now.", false, "mostly English"},
		{"digits are not letters", "It costs 100 dollars.", "它要 100 美元。", true, ""},

		// rule 4: length ratio
		{"too short", "This is a rather long sentence about caches and memory systems today.", "好。", false, "length ratio"},
		{"too long", "Okay.", "好的，那我们现在就正式开始今天的课程内容吧。", false, "length ratio"},
		{"lower bound inclusive", "one two three four", "好的", true, ""},          // 2/4 = 0.5
		{"upper bound inclusive", "Okay", "好的好的", true, ""},                      // 4/1 = 4
		{"punctuation-only tokens not words", "Yes — right.", "是的，对。", true, ""}, // 5 chars / 2 words

		// rule 5: numbers
		{"numbers kept", "The miss rate drops from 5 percent to about 3 percent.", "缺失率从 5% 降到大约 3%。", true, ""},
		{"number missing", "Each line is 64 bytes.", "每行是六十四字节。", false, "the number 64 from the source sentence is missing"},
		{"several missing", "From 5 to 3 percent.", "从五降到三个百分点。", false, "the numbers 5, 3 from the source sentence are missing"},
		{"no partial digit match", "About 5 percent.", "大约 15%。", false, "number 5"},
		{"decimal", "Pi is about 3.14 here.", "π 大约是 3.14。", true, ""},
		{"decimal changed", "Pi is about 3.14 here.", "π 大约是 3。", false, "number 3.14"},
		{"thousands separator", "We have 1,000 lines.", "我们有 1000 行。", true, ""},
		{"full-width digits", "We have 64 lines.", "我们有６４行。", true, ""},
		{"digits inside a token", "Most x86 machines.", "大多数 x86 机器。", true, ""},
		{"list with Chinese comma", "Sizes 5, 10 and 200 bytes.", "大小为 5，10 和 200 字节。", true, ""},

		// several rules at once
		{"multiple failures joined", "Each cache line is 64 bytes.", "Each line is many bytes ok", false, "; "},
	}
	for _, c := range cases {
		ok, reason := Check(c.target, c.zh, glossary)
		if ok != c.ok {
			t.Errorf("%s: ok=%v reason=%q, want ok=%v", c.name, ok, reason, c.ok)
			continue
		}
		if ok && reason != "" {
			t.Errorf("%s: ok with reason %q", c.name, reason)
		}
		if !ok && !strings.Contains(reason, c.reason) {
			t.Errorf("%s: reason %q lacks %q", c.name, reason, c.reason)
		}
		if !ok && strings.HasSuffix(reason, ".") {
			t.Errorf("%s: reason must not end with a period (it is followed by \". Translate again.\"): %q", c.name, reason)
		}
	}
}

// TestShortTargetLengthRatio: for targets of at most ShortTargetWords
// English words rule 4 skips the lower bound and computes the upper bound
// for ShortTargetWords+1 words, i.e. at most MaxLengthRatio*(ShortTargetWords+1)
// = 16 characters (amendment of SPEC §7.4 item 4, from the real acceptance
// run: "Okay?" / "Anything desirable?" were rejected). The other rules still
// apply to short targets; from 4 words on rule 4 is unchanged.
func TestShortTargetLengthRatio(t *testing.T) {
	const zh16 = "一二三四五六七八九十一二三四五六"
	cases := []struct {
		name, target, zh string
		want             []string // failure codes
	}{
		// 1-3 words: upper bound 16 characters, no lower bound
		{"1 word, acceptance case", "Okay?", "好的，明白了吗？", nil},                // 8
		{"2 words, acceptance case", "Anything desirable?", "有什么想要的吗？", nil}, // 8
		{"1 word at 16", "Okay?", zh16, nil},
		{"1 word at 17", "Okay?", zh16 + "七", []string{CodeLengthRatio}},
		{"2 words at 16", "Anything desirable?", zh16, nil},
		{"2 words at 17", "Anything desirable?", zh16 + "七", []string{CodeLengthRatio}},
		{"3 words at 16", "Is that clear?", zh16, nil},
		{"3 words at 17", "Is that clear?", zh16 + "七", []string{CodeLengthRatio}},
		{"1 word, one character", "Okay?", "好", nil},
		{"3 words, one character", "Is that clear?", "清", nil},
		{"contraction is one word", "Don't worry, okay?", "大家千万不要担心，好不好呀？", nil}, // 3 words, 14
		{"hyphenated word is one word", "Well-known result.", "这是一个大家都熟知的结论。", nil},
		{"number is a word", "Page 42.", "请大家翻到第 42 页看一下。", nil},
		{"punctuation tokens are not words", "So — okay — right?", "所以说，好的，对吧，没问题吧？", nil},                // 3 words, 14
		{"1 word expanded into a sentence", "Okay.", "好的，那我们现在就正式开始今天的课程内容吧。", []string{CodeLengthRatio}}, // 22

		// other rules still apply to short targets
		{"empty still fails", "Okay?", "", []string{CodeEmpty}},
		{"English residue still fails", "Is that clear?", "Is that clear?", []string{CodeMostlyEnglish}},
		{"term still enforced", "The cache.", "那个高速存储器。", []string{CodeTermMissing}},
		{"number still checked", "Page 42.", "请大家翻到那一页。", []string{CodeNumberMissing}},

		// 4 words and more: unchanged
		{"4 words over the upper bound", "Is that clear now?", "大家现在对这个都已经清楚明白了吗？", []string{CodeLengthRatio}},         // 17/4
		{"4 words under the lower bound", "one two three four", "好", []string{CodeLengthRatio}},                        // 1/4
		{"4 words at the upper bound", "one two three four", zh16, nil},                                                // 16/4
		{"contraction counted once at 4 words", "Don't you see it?", "你难道真的就是一点都没有看出来这个吗？", []string{CodeLengthRatio}}, // 18/4
	}
	for _, c := range cases {
		var got []string
		for _, f := range Failures(c.target, c.zh, glossary) {
			got = append(got, f.Code)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: failures %v, want %v", c.name, got, c.want)
		}
	}
	// the reported ratio uses the same word count as the check (22/4), so it
	// lies outside [min, max] exactly when the check fails
	if _, reason := Check("Okay.", "好的，那我们现在就正式开始今天的课程内容吧。", nil); !strings.Contains(reason, "the length ratio is 5.50 characters per English word, outside [0.5, 4]") {
		t.Errorf("short target reason %q", reason)
	}
	for _, c := range []struct {
		s string
		n int
	}{
		{"Okay?", 1}, {"Don't worry.", 2}, {"well-known", 1}, {"Page 42 , 3.14 !", 3}, {"— ? …", 0}, {"rock 'n' roll", 3},
	} {
		if n := EnglishWords(c.s); n != c.n {
			t.Errorf("EnglishWords(%q) = %d, want %d", c.s, n, c.n)
		}
	}
}

func TestMetrics(t *testing.T) {
	if r := LatinRatio("abc缓存", nil); r != 0.6 {
		t.Errorf("LatinRatio = %v", r)
	}
	if r := LatinRatio("LRU 替换", []string{"LRU"}); r != 0 {
		t.Errorf("LatinRatio with English-only term = %v", r)
	}
	if r := LatinRatio("123，。", nil); r != 0 {
		t.Errorf("LatinRatio without letters = %v", r)
	}
	if r := CodeSymbolRatio("a=b;"); r != 0.5 {
		t.Errorf("CodeSymbolRatio = %v", r)
	}
	if n := ZhChars(" 缓 存 行。"); n != 4 {
		t.Errorf("ZhChars = %d", n)
	}
	if n := EnglishWords("Hello, — world 42 !"); n != 3 {
		t.Errorf("EnglishWords = %d", n)
	}
	if got := Numbers("v2.7.1 has 1,000 and 1,000 and 3.5 and x86"); !slices.Equal(got, []string{"2.7", "1", "1000", "3.5", "86"}) {
		t.Errorf("Numbers = %q", got)
	}
}

// TestPackageIsPure enforces SPEC §1.1: no I/O, clock or randomness in qa.
func TestPackageIsPure(t *testing.T) {
	bad := []string{"os", "os/exec", "io/ioutil", "io/fs", "net", "time", "math/rand", "math/rand/v2", "crypto/rand",
		"database/sql", "syscall", "log", "log/slog", "path/filepath", "runtime",
		"lectureflow/internal/db", "lectureflow/internal/llm", "lectureflow/internal/sched"}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if slices.Contains(bad, p) || strings.HasPrefix(p, "net/") {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
}
