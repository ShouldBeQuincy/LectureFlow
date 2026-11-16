// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

// SPEC-v2 P14.9 checks of the UI text:
//
//   - hard-coded text: no CJK character in web/ outside i18n/ and the
//     third-party vendor/ directory (.js, .html and .css; comments are
//     skipped), reported literal by literal; no
//     suspicious English UI sentence (a string literal starting with a
//     capital letter and containing a space, or a template literal starting
//     with ${…} followed by an English phrase) in a place where UI text goes:
//     textContent/title/placeholder/aria* assignments, dataset.tip,
//     setAttribute('aria-label'|'aria-description'|'aria-valuetext'|
//     'data-tip'|'title'|'placeholder', …), the text, title, label, hint,
//     message, placeholder, tip, aria-* … properties of an object literal,
//     the child arguments of h()/el() (arrays included), the first argument
//     of btn(), toast(), openDialog(), errorBlock(), alert(), confirm() and
//     prompt(); in CSS, the strings of content:. Exceptions are listed in
//     i18n/hardcode_allowlist.txt as "file:fragment  # reason", the fragment
//     being exactly the reported literal;
//   - key completeness: every catalog has the keys of zh-CN.json; en.json is
//     complete, the other languages miss at most 2 % (absent files skip);
//   - parameter consistency: a key has the same {params} in every language
//     (each plural form compared on its own);
//   - plural forms: every plural object has each category that
//     Intl.PluralRules selects in its language (pluralCategories);
//   - every literal key passed to t('…') exists in zh-CN.json (keys built
//     at run time are skipped);
//   - formats: numbers and dates are formatted by util.js only (P14.4).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

const (
	catalogDir    = "i18n"
	sourceLocale  = "zh-CN"
	allowlistPath = "i18n/hardcode_allowlist.txt"
	// vendorDir holds third-party code copied unchanged (opencc-js for
	// P14.6, see vendor/opencc-js/README.md). It is not our UI code: its
	// conversion dictionaries are Chinese text by design and it never
	// calls t(), so the checks below skip it (only this directory).
	vendorDir = "vendor"
)

// ---- catalogs ----

// catalog is one web/i18n/{locale}.json: key → string or plural object.
type catalog map[string]any

// loadCatalog reads a catalog and reports duplicate keys (agents insert
// lines into shared files; json.Unmarshal would silently keep the last).
func loadCatalog(t *testing.T, path string) catalog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if dups := duplicateKeys(data); len(dups) > 0 {
		t.Errorf("%s: duplicate keys: %s", path, strings.Join(dups, ", "))
	}
	var c catalog
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

// duplicateKeys lists the keys that occur more than once at the top level.
func duplicateKeys(data []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	var dups []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return dups
		}
		k, _ := tok.(string)
		if seen[k] {
			dups = append(dups, k)
		}
		seen[k] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return dups
		}
	}
	return dups
}

// textKeys are the UI text keys of a catalog (metadata and the "_section.*"
// anchors start with "_").
func (c catalog) textKeys() map[string]bool {
	out := map[string]bool{}
	for k := range c {
		if !strings.HasPrefix(k, "_") {
			out[k] = true
		}
	}
	return out
}

// otherLocales are the catalog files besides zh-CN.json (context.json is
// not a language).
func otherLocales(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || name == "context.json" || name == sourceLocale+".json" {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(out)
	return out
}

func TestI18nKeyCompleteness(t *testing.T) {
	src := loadCatalog(t, filepath.Join(catalogDir, sourceLocale+".json"))
	want := src.textKeys()
	locales := otherLocales(t)
	if !contains(locales, "en") {
		t.Errorf("%s/en.json is missing (en must be complete, P14.9)", catalogDir)
	}
	for _, loc := range locales {
		got := loadCatalog(t, filepath.Join(catalogDir, loc+".json")).textKeys()
		var missing, extra []string
		for k := range want {
			if !got[k] {
				missing = append(missing, k)
			}
		}
		for k := range got {
			if !want[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if loc == "en" {
			if len(missing) > 0 {
				t.Errorf("en.json misses %d keys of zh-CN.json: %s", len(missing), strings.Join(missing, ", "))
			}
			if len(extra) > 0 {
				t.Errorf("en.json has %d keys that zh-CN.json does not have: %s", len(extra), strings.Join(extra, ", "))
			}
			continue
		}
		limit := len(want) * 2 / 100
		if len(missing) > limit {
			t.Errorf("%s.json misses %d keys (more than 2%% = %d): %s", loc, len(missing), limit, strings.Join(missing, ", "))
		} else if len(missing) > 0 {
			t.Logf("%s.json misses %d keys (allowed up to %d): %s", loc, len(missing), limit, strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			t.Logf("%s.json has %d keys that zh-CN.json does not have: %s", loc, len(extra), strings.Join(extra, ", "))
		}
	}
	// Usage context (P14.8.1 step 2): reported, not enforced here.
	if ctxPath := filepath.Join(catalogDir, "context.json"); fileExists(ctxPath) {
		ctx := loadCatalog(t, ctxPath).textKeys()
		var missing []string
		for k := range want {
			if !ctx[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Logf("context.json has no entry for %d keys: %s", len(missing), strings.Join(missing, ", "))
		}
	}
}

// placeholderRe matches {param} and {param:modifier}. The one modifier t()
// knows is lc (round 9 WH-10, i18n.js: the value's words lower-cased at
// their first letter, for a fragment in the middle of a Russian sentence);
// {wait:lc} is the same param as {wait}.
var placeholderRe = regexp.MustCompile(`\{(\w+)(?::(\w+))?\}`)

// placeholderMods are the modifiers t() knows.
var placeholderMods = map[string]bool{"lc": true}

// paramSets returns the {param} set of each form of a value: "" for a plain
// string, the plural category otherwise. {param:lc} counts as {param}; an
// unknown modifier is an error (t() would leave it as written).
func paramSets(v any) (map[string][]string, error) {
	set := func(s string) ([]string, error) {
		seen := map[string]bool{}
		var out []string
		for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
			if m[2] != "" && !placeholderMods[m[2]] {
				return nil, fmt.Errorf("placeholder %s has an unknown modifier %q (only :lc)", m[0], m[2])
			}
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
		sort.Strings(out)
		return out, nil
	}
	switch x := v.(type) {
	case string:
		ps, err := set(x)
		if err != nil {
			return nil, err
		}
		return map[string][]string{"": ps}, nil
	case map[string]any:
		out := map[string][]string{}
		for form, s := range x {
			str, ok := s.(string)
			if !ok {
				return nil, fmt.Errorf("plural form %q is not a string", form)
			}
			ps, err := set(str)
			if err != nil {
				return nil, fmt.Errorf("plural form %q: %w", form, err)
			}
			out[form] = ps
		}
		return out, nil
	}
	return nil, fmt.Errorf("value is neither a string nor a plural object")
}

// The back end sends texts as codes (P14.3): every code of the API's catalog
// (testdata/api/error_codes.json, shown as errors.{code}) and of the other
// back-end packages (error_codes_backend.json, errors / notes / warnings by
// its kind) has its key in zh-CN.json and en.json, whose {params} are ones
// the back end sends (a placeholder it never fills would show as "{x}").
// A code's listed extra sentences (params.extra) are codes of the same
// kind. Params the text leaves out (retries, cause) are allowed.
func TestI18nBackendCodesHaveTexts(t *testing.T) {
	type entry struct {
		Params []string `json:"params"`
		Kind   string   `json:"kind"`
		Extra  []string `json:"extra"`
	}
	read := func(name string) map[string]json.RawMessage {
		data, err := os.ReadFile(filepath.Join("..", "testdata", "api", name))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m
	}
	ns := map[string]string{"error": "errors", "note": "notes", "warning": "warnings"}
	type code struct {
		file, key string
		params    []string
		extra     []string
	}
	var codes []code
	for _, f := range []string{"error_codes.json", "error_codes_backend.json"} {
		for c, raw := range read(f) {
			if strings.HasPrefix(c, "_") {
				continue
			}
			var e entry
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("%s %s: %v", f, c, err)
			}
			area := "errors"
			if f == "error_codes_backend.json" {
				if area = ns[e.Kind]; area == "" {
					t.Errorf("%s %s: unknown kind %q", f, c, e.Kind)
					continue
				}
			}
			var extra []string
			for _, x := range e.Extra {
				extra = append(extra, area+"."+x)
			}
			codes = append(codes, code{file: f, key: area + "." + c, params: e.Params, extra: extra})
		}
	}
	if len(codes) < 100 {
		t.Fatalf("only %d codes read", len(codes))
	}
	known := map[string]bool{}
	for _, c := range codes {
		known[c.key] = true
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i].key < codes[j].key })
	for _, loc := range []string{sourceLocale, "en"} {
		cat := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		for _, c := range codes {
			v, ok := cat[c.key]
			if !ok {
				t.Errorf("%s.json has no %s (%s)", loc, c.key, c.file)
				continue
			}
			forms, err := paramSets(v)
			if err != nil {
				t.Errorf("%s.json %s: %v", loc, c.key, err)
				continue
			}
			sent := map[string]bool{}
			for _, p := range c.params {
				sent[p] = true
			}
			for form, ps := range forms {
				for _, p := range ps {
					if !sent[p] {
						t.Errorf("%s.json %s%s: {%s} is not a param of the code (%s lists %v)", loc, c.key, map[bool]string{true: "", false: "." + form}[form == ""], p, c.file, c.params)
					}
				}
			}
		}
	}
	for _, c := range codes {
		for _, x := range c.extra {
			if !known[x] {
				t.Errorf("%s: extra sentence %s is not a code of the same kind", c.key, x)
			}
		}
	}
}

func TestI18nParamConsistency(t *testing.T) {
	src := loadCatalog(t, filepath.Join(catalogDir, sourceLocale+".json"))
	ref := map[string][]string{}
	for k := range src.textKeys() {
		forms, err := paramSets(src[k])
		if err != nil {
			t.Errorf("zh-CN.json %s: %v", k, err)
			continue
		}
		// zh-CN only needs "other"; all its forms must agree.
		var first []string
		firstSet := false
		for form, ps := range forms {
			if !firstSet {
				first, firstSet = ps, true
			} else if strings.Join(ps, ",") != strings.Join(first, ",") {
				t.Errorf("zh-CN.json %s: form %q has params %v, another form has %v", k, form, ps, first)
			}
		}
		ref[k] = first
	}
	for _, loc := range otherLocales(t) {
		c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		for k := range c.textKeys() {
			want, ok := ref[k]
			if !ok {
				continue // reported by TestI18nKeyCompleteness
			}
			forms, err := paramSets(c[k])
			if err != nil {
				t.Errorf("%s.json %s: %v", loc, k, err)
				continue
			}
			for form, ps := range forms {
				if strings.Join(ps, ",") != strings.Join(want, ",") {
					name := k
					if form != "" {
						name += "." + form
					}
					t.Errorf("%s.json %s: params %v, zh-CN has %v", loc, name, ps, want)
				}
			}
		}
	}
}

// ---- sources ----

// sourceFile is a .js, .html or .css file of web/ outside i18n/ and vendor/.
type sourceFile struct {
	path string // slash-separated, relative to web/
	src  string
}

func sourceFiles(t *testing.T) []sourceFile {
	t.Helper()
	var out []sourceFile
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == catalogDir || p == vendorDir || (p != "." && strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(p)
		if ext != ".js" && ext != ".html" && ext != ".css" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, sourceFile{path: filepath.ToSlash(p), src: string(data)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ---- a small JavaScript lexer ----

// jsToken is a token of the lexer: 'i' identifier or keyword, 'n' number,
// 'p' punctuator, 's' string literal, 't' template literal (its ${…}
// parts become "{}"), 'r' regular expression literal.
type jsToken struct {
	kind byte
	val  string
	line int
}

// lexJS returns the source with comments blanked out (line breaks kept, so
// line numbers stay right) and its tokens. It knows enough JavaScript for
// the front end: strings, nested template literals, regular expression
// literals (told from division by the previous token) and comments.
func lexJS(src string) (string, []jsToken) {
	rs := []rune(src)
	out := make([]rune, len(rs))
	copy(out, rs)
	var toks []jsToken
	line := 1
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	// template frames: each ${ … } expression remembers its brace depth and
	// where its template token goes.
	type frame struct {
		depth   int
		text    []rune
		tokAt   int
		startLn int
	}
	var frames []frame
	regexAllowed := func() bool {
		if len(toks) == 0 {
			return true
		}
		last := toks[len(toks)-1]
		switch last.kind {
		case 'n', 's', 't', 'r':
			return false
		case 'i':
			switch last.val {
			case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "yield", "await", "instanceof":
				return true
			}
			return false
		case 'p':
			return last.val != ")" && last.val != "]"
		}
		return true
	}
	// lexTemplate reads template text from i (after ` or }) up to the
	// closing ` (returns false) or a ${ (returns true, with i after it).
	lexTemplate := func(i *int, f *frame) bool {
		for *i < len(rs) {
			c := rs[*i]
			switch {
			case c == '\\' && *i+1 < len(rs):
				f.text = append(f.text, rs[*i+1])
				if rs[*i+1] == '\n' {
					line++
				}
				*i += 2
			case c == '`':
				*i++
				return false
			case c == '$' && *i+1 < len(rs) && rs[*i+1] == '{':
				f.text = append(f.text, '{', '}')
				*i += 2
				return true
			default:
				if c == '\n' {
					line++
				}
				f.text = append(f.text, c)
				*i++
			}
		}
		return false
	}
	finishTemplate := func(f frame) {
		tok := jsToken{kind: 't', val: string(f.text), line: f.startLn}
		toks = append(toks, jsToken{})
		copy(toks[f.tokAt+1:], toks[f.tokAt:])
		toks[f.tokAt] = tok
	}
	startTemplate := func(i *int) {
		f := frame{tokAt: len(toks), startLn: line}
		*i++
		if lexTemplate(i, &f) {
			frames = append(frames, f)
		} else {
			finishTemplate(f)
		}
	}
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case c == '\n':
			line++
			i++
		case unicode.IsSpace(c):
			i++
		case c == '/' && i+1 < len(rs) && rs[i+1] == '/':
			for i < len(rs) && rs[i] != '\n' {
				blank(i)
				i++
			}
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			blank(i)
			blank(i + 1)
			i += 2
			for i < len(rs) && !(rs[i] == '*' && i+1 < len(rs) && rs[i+1] == '/') {
				if rs[i] == '\n' {
					line++
				}
				blank(i)
				i++
			}
			if i < len(rs) {
				blank(i)
				blank(i + 1)
				i += 2
			}
		case c == '\'' || c == '"':
			start := line
			var b []rune
			i++
			for i < len(rs) && rs[i] != c && rs[i] != '\n' {
				if rs[i] == '\\' && i+1 < len(rs) {
					b = append(b, rs[i+1])
					i += 2
					continue
				}
				b = append(b, rs[i])
				i++
			}
			i++
			toks = append(toks, jsToken{kind: 's', val: string(b), line: start})
		case c == '`':
			startTemplate(&i)
		case c == '/' && regexAllowed():
			start := line
			j := i + 1
			inClass := false
			for j < len(rs) && rs[j] != '\n' {
				if rs[j] == '\\' {
					j += 2
					continue
				}
				if rs[j] == '[' {
					inClass = true
				} else if rs[j] == ']' {
					inClass = false
				} else if rs[j] == '/' && !inClass {
					break
				}
				j++
			}
			j++
			for j < len(rs) && (unicode.IsLetter(rs[j])) {
				j++
			}
			if j > len(rs) {
				j = len(rs)
			}
			toks = append(toks, jsToken{kind: 'r', val: string(rs[i:j]), line: start})
			i = j
		case c == '_' || c == '$' || unicode.IsLetter(c):
			j := i
			for j < len(rs) && (rs[j] == '_' || rs[j] == '$' || unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
				j++
			}
			toks = append(toks, jsToken{kind: 'i', val: string(rs[i:j]), line: line})
			i = j
		case unicode.IsDigit(c) || (c == '.' && i+1 < len(rs) && unicode.IsDigit(rs[i+1])):
			j := i
			for j < len(rs) && (unicode.IsDigit(rs[j]) || unicode.IsLetter(rs[j]) || rs[j] == '.' || rs[j] == '_') {
				j++
			}
			toks = append(toks, jsToken{kind: 'n', val: string(rs[i:j]), line: line})
			i = j
		case c == '}' && len(frames) > 0 && frames[len(frames)-1].depth == 0:
			// end of a ${ … } expression: back into the template text
			i++
			f := frames[len(frames)-1]
			frames = frames[:len(frames)-1]
			if lexTemplate(&i, &f) {
				frames = append(frames, f)
			} else {
				finishTemplate(f)
			}
		default:
			op := string(c)
			for _, multi := range []string{"===", "!==", "...", "=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "+=", "-="} {
				if strings.HasPrefix(string(rs[i:min(i+len(multi), len(rs))]), multi) {
					op = multi
					break
				}
			}
			if op == "?." && i+2 < len(rs) && unicode.IsDigit(rs[i+2]) {
				op = "?"
			}
			if len(frames) > 0 {
				if op == "{" {
					frames[len(frames)-1].depth++
				} else if op == "}" {
					frames[len(frames)-1].depth--
				}
			}
			toks = append(toks, jsToken{kind: 'p', val: op, line: line})
			i += len([]rune(op))
		}
	}
	return string(out), toks
}

// ---- hard-coded text ----

// isCJK: Han ideographs, CJK punctuation, kana, Hangul and full-width forms.
func isCJK(r rune) bool {
	switch {
	case r >= 0x3000 && r <= 0x303F, r >= 0x3040 && r <= 0x30FF, r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF, r >= 0xAC00 && r <= 0xD7AF, r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFF00 && r <= 0xFFEF, r >= 0x20000 && r <= 0x3FFFF:
		return true
	}
	return false
}

func hasCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// englishSentenceRe: a capital letter first, a space later, and some
// lower-case letters ("Save changes", not "GPU" or "MOCK").
var englishSentenceRe = regexp.MustCompile(`^[A-Z][A-Za-z'’]*[a-z][A-Za-z'’]*[,.:;!?]?\s+[A-Za-z({]`)

// englishPhraseRe: a word of two or more letters in either case, then a
// space and another word ("files uploaded").
var englishPhraseRe = regexp.MustCompile(`^[A-Za-z][A-Za-z'’]*[a-z][A-Za-z'’]*[,.:;!?]?\s+[A-Za-z({]`)

func looksLikeSentence(s string) bool { return englishSentenceRe.MatchString(s) }

// looksLikeUIText: a sentence, or a template literal that starts with ${…}
// followed by an English phrase (`${n} files uploaded`): such a template
// does not start with a capital letter, but it is UI text all the same.
func looksLikeUIText(tk jsToken) bool {
	if looksLikeSentence(tk.val) {
		return true
	}
	if tk.kind == 't' && strings.HasPrefix(tk.val, "{}") {
		return englishPhraseRe.MatchString(strings.TrimLeft(tk.val, "{} \t\n·:;,.-–—/|()[]×"))
	}
	return false
}

// UI text slots of an object literal, of an assignment and of setAttribute.
var (
	uiProps = map[string]bool{
		"text": true, "title": true, "label": true, "hint": true, "message": true, "placeholder": true,
		"aria-label": true, "ariaLabel": true, "aria-description": true, "ariaDescription": true,
		"aria-valuetext": true, "ariaValueText": true, "data-tip": true, "tip": true,
		"okText": true, "cancelText": true, "buttonText": true, "alt": true,
	}
	uiMembers = map[string]bool{"textContent": true, "innerText": true, "title": true, "placeholder": true, "ariaLabel": true, "ariaDescription": true, "ariaValueText": true, "alt": true}
	uiAttrs   = map[string]bool{"aria-label": true, "aria-description": true, "aria-valuetext": true, "data-tip": true, "title": true, "placeholder": true, "alt": true}
)

// enclosing returns the opener ("(", "[" or "{") of the list that toks[k]
// ("(", "[" or ",") belongs to, and the index of the item that follows k
// in that list; -1 when there is none.
func enclosing(toks []jsToken, k int) (open, arg int) {
	if toks[k].val != "," {
		return k, 0
	}
	arg, d := 1, 0
	for open = k - 1; open >= 0; open-- {
		v := toks[open]
		if v.kind != 'p' {
			continue
		}
		switch v.val {
		case ")", "]", "}":
			d++
		case "(", "[", "{":
			if d == 0 {
				return open, arg
			}
			d--
		case ",":
			if d == 0 {
				arg++
			}
		}
	}
	return -1, 0
}

// isArrayLiteral: the "[" at toks[k] starts an array literal, not a member
// access (told apart by the previous token, like a regular expression).
func isArrayLiteral(toks []jsToken, k int) bool {
	if k == 0 {
		return true
	}
	prev := toks[k-1]
	switch prev.kind {
	case 'p':
		return prev.val != ")" && prev.val != "]" && prev.val != "}"
	case 'i':
		switch prev.val {
		case "return", "typeof", "case", "do", "else", "in", "of", "new", "void", "throw", "yield", "await":
			return true
		}
	}
	return false
}

// uiSlot reports whether toks[i] (a string or template literal) is in a
// place where UI text goes, and which one. It walks back over the
// expression the literal is part of (ternaries included) to the token that
// starts it: "{" or "," (an object property when "key:" follows it), "=",
// the "(" / "," of a call, or the "[" / "," of an array literal (the array
// is then judged by where it stands: the children of h() may be arrays).
func uiSlot(toks []jsToken, i int) (bool, string) {
	depth, firstColon, stop := 0, -1, -1
walk:
	for k := i - 1; k >= 0; k-- {
		tk := toks[k]
		if tk.kind == 'i' && depth == 0 && tk.val == "return" {
			stop = k
			break
		}
		if tk.kind != 'p' {
			continue
		}
		switch tk.val {
		case ")", "]", "}":
			depth++
			continue
		case "(", "[", "{":
			if depth > 0 {
				depth--
				continue
			}
			stop = k
			break walk
		}
		if depth > 0 {
			continue
		}
		switch tk.val {
		case ":":
			firstColon = k
		case ",", ";", "=", "=>", "+=":
			stop = k
			break walk
		}
	}
	if stop < 0 {
		return false, ""
	}
	// "{ key: …" or ", key: …": an object property (a ternary's ":" never
	// directly follows the key after the start token)
	if firstColon >= 2 && firstColon-2 == stop && (toks[stop].val == "{" || toks[stop].val == ",") {
		key := toks[firstColon-1]
		if (key.kind == 'i' || key.kind == 's') && uiProps[key.val] {
			return true, "property " + key.val
		}
		return false, ""
	}
	switch toks[stop].val {
	case "=", "+=":
		if stop >= 2 && toks[stop-1].kind == 'i' && toks[stop-2].val == "." && uiMembers[toks[stop-1].val] {
			return true, "." + toks[stop-1].val
		}
		if stop >= 4 && toks[stop-1].val == "tip" && toks[stop-2].val == "." && toks[stop-3].val == "dataset" {
			return true, ".dataset.tip"
		}
	case "(", ",", "[":
		open, arg := enclosing(toks, stop)
		if open < 0 {
			return false, ""
		}
		if toks[open].val == "[" {
			if !isArrayLiteral(toks, open) {
				return false, ""
			}
			if ok, why := uiSlot(toks, open); ok {
				return true, "array in " + why
			}
			return false, ""
		}
		if open < 1 || toks[open].val != "(" || toks[open-1].kind != 'i' {
			return false, ""
		}
		switch callee := toks[open-1].val; {
		case (callee == "h" || callee == "el") && arg >= 2:
			return true, callee + "() child"
		case (callee == "btn" || callee == "toast" || callee == "openDialog" || callee == "errorBlock") && arg == 0:
			return true, callee + "()"
		case (callee == "alert" || callee == "confirm" || callee == "prompt") && arg == 0:
			return true, callee + "()"
		case callee == "iconBtn" && arg == 1:
			return true, "iconBtn()"
		case (callee == "append" || callee == "prepend" || callee == "createTextNode" || callee == "replaceChildren") && open >= 2 && toks[open-2].val == ".":
			return true, callee + "()"
		case callee == "setAttribute" && arg == 1 && toks[open+1].kind == 's' && uiAttrs[toks[open+1].val]:
			return true, "setAttribute('" + toks[open+1].val + "')"
		}
	}
	return false, ""
}

// finding is one hard-coded text: for CJK the fragment itself (a string,
// template or regular expression literal, an HTML text node or attribute
// value, a CSS string), so the allowlist names exactly what it allows.
type finding struct {
	file, text, why string
	line            int
}

func (f finding) String() string { return fmt.Sprintf("%s:%d: %s (%s)", f.file, f.line, f.text, f.why) }

func scanJS(file, src string) []finding { return scanJSAt(file, src, 0) }

// scanJSAt scans JavaScript whose first line is line offset+1 of file (the
// <script> of an HTML page). Comments are not tokens, so they are skipped.
func scanJSAt(file, src string, offset int) []finding {
	_, toks := lexJS(src)
	var out []finding
	for i, tk := range toks {
		if hasCJK(tk.val) {
			out = append(out, finding{file: file, line: tk.line + offset, text: tk.val, why: "CJK"})
			continue
		}
		if (tk.kind != 's' && tk.kind != 't') || !looksLikeUIText(tk) {
			continue
		}
		if ok, why := uiSlot(toks, i); ok {
			out = append(out, finding{file: file, line: tk.line + offset, text: tk.val, why: "English UI text in " + why})
		}
	}
	return out
}

var (
	cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssStringRe  = regexp.MustCompile(`"((?:[^"\\\n]|\\.)*)"|'((?:[^'\\\n]|\\.)*)'`)
	cssContentRe = regexp.MustCompile(`(?i)(?:^|[\s;{])content\s*:([^;}]*)`)
)

// scanCSSAt scans a style sheet whose first line is line offset+1 of file:
// CJK anywhere outside comments, and English sentences in the strings of
// `content:` (text that CSS puts on the page, e.g. a ::after label; it
// belongs in a data-* attribute filled with t(), shown with attr()).
func scanCSSAt(file, src string, offset int) []finding {
	s := cssCommentRe.ReplaceAllStringFunc(src, blankKeepLines)
	lineOf := func(off int) int { return strings.Count(s[:off], "\n") + 1 + offset }
	content := cssContentRe.FindAllStringSubmatchIndex(s, -1)
	inContent := func(off int) bool {
		for _, m := range content {
			if off >= m[2] && off < m[3] {
				return true
			}
		}
		return false
	}
	var out []finding
	rest := []byte(s)
	for _, m := range cssStringRe.FindAllStringSubmatchIndex(s, -1) {
		v := ""
		if m[2] >= 0 {
			v = s[m[2]:m[3]]
		} else {
			v = s[m[4]:m[5]]
		}
		blankRange(rest, m[0], m[1])
		switch {
		case hasCJK(v):
			out = append(out, finding{file: file, line: lineOf(m[0]), text: v, why: "CJK"})
		case inContent(m[0]) && looksLikeSentence(v):
			out = append(out, finding{file: file, line: lineOf(m[0]), text: v, why: "English UI text in CSS content"})
		}
	}
	for n, ln := range strings.Split(string(rest), "\n") {
		if hasCJK(ln) {
			out = append(out, finding{file: file, line: n + 1 + offset, text: strings.TrimSpace(ln), why: "CJK"})
		}
	}
	return out
}

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlScriptRe  = regexp.MustCompile(`(?is)(<script\b[^>]*>)(.*?)(</script>)`)
	htmlStyleRe   = regexp.MustCompile(`(?is)(<style\b[^>]*>)(.*?)(</style>)`)
	htmlAttrRe    = regexp.MustCompile(`\s([\w:.-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	htmlTextRe    = regexp.MustCompile(`>([^<>]+)<`)
)

// blankKeepLines replaces s with spaces, keeping its line breaks.
func blankKeepLines(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		return ' '
	}, s)
}

// blankRange overwrites b[from:to] with spaces byte by byte, keeping line
// breaks, so the byte offsets of the rest stay valid.
func blankRange(b []byte, from, to int) {
	for i := from; i < to; i++ {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
}

// scanHTML: scripts with the JavaScript scanner, styles with the CSS
// scanner; in the markup, CJK in any attribute value or text node and
// English sentences in the UI attributes and text nodes.
func scanHTML(file, src string) []finding {
	s := htmlCommentRe.ReplaceAllStringFunc(src, blankKeepLines)
	lineOf := func(off int) int { return strings.Count(s[:off], "\n") + 1 }
	var out []finding
	markup := []byte(s)
	for _, m := range htmlScriptRe.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, scanJSAt(file, s[m[4]:m[5]], lineOf(m[4])-1)...)
		blankRange(markup, m[4], m[5])
	}
	for _, m := range htmlStyleRe.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, scanCSSAt(file, s[m[4]:m[5]], lineOf(m[4])-1)...)
		blankRange(markup, m[4], m[5])
	}
	ms := string(markup)
	rest := []byte(ms)
	for _, m := range htmlAttrRe.FindAllStringSubmatchIndex(ms, -1) {
		vs, ve := m[4], m[5]
		if vs < 0 {
			vs, ve = m[6], m[7]
		}
		name, v := ms[m[2]:m[3]], ms[vs:ve]
		blankRange(rest, vs, ve)
		switch {
		case hasCJK(v):
			out = append(out, finding{file: file, line: lineOf(vs), text: v, why: "CJK"})
		case uiAttrs[name] && looksLikeSentence(v):
			out = append(out, finding{file: file, line: lineOf(m[0]), text: v, why: "English UI text in attribute " + name})
		}
	}
	for _, m := range htmlTextRe.FindAllStringSubmatchIndex(ms, -1) {
		raw := ms[m[2]:m[3]]
		v := strings.TrimSpace(raw)
		blankRange(rest, m[2], m[3])
		switch {
		case hasCJK(v):
			out = append(out, finding{file: file, line: lineOf(m[2] + strings.Index(raw, v)), text: v, why: "CJK"})
		case looksLikeSentence(v):
			out = append(out, finding{file: file, line: lineOf(m[2]), text: v, why: "English UI text in markup"})
		}
	}
	// CJK anywhere else in the markup (a name, an unquoted value): the line
	for n, ln := range strings.Split(string(rest), "\n") {
		if hasCJK(ln) {
			out = append(out, finding{file: file, line: n + 1, text: strings.TrimSpace(ln), why: "CJK"})
		}
	}
	return out
}

// allowEntry is a line "file:fragment  # reason" of the allowlist.
type allowEntry struct {
	file, fragment string
	used           bool
}

// allows: the entry names the finding's file and exactly its text (a CJK
// literal of a line that also holds other CJK is not allowed by an entry
// for its neighbour).
func (a *allowEntry) allows(f finding) bool {
	return a.file == f.file && a.fragment == strings.TrimSpace(f.text)
}

func loadAllowlist(t *testing.T) []*allowEntry {
	t.Helper()
	data, err := os.ReadFile(allowlistPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return parseAllowlist(t, string(data))
}

func parseAllowlist(t *testing.T, data string) []*allowEntry {
	t.Helper()
	var out []*allowEntry
	for n, ln := range strings.Split(data, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		body, reason, ok := strings.Cut(ln, "  #")
		file, fragment, ok2 := strings.Cut(body, ":")
		if !ok || strings.TrimSpace(reason) == "" || !ok2 || strings.TrimSpace(fragment) == "" {
			t.Errorf("%s:%d: want \"file:fragment  # reason\", got %q", allowlistPath, n+1, ln)
			continue
		}
		out = append(out, &allowEntry{file: strings.TrimSpace(file), fragment: strings.TrimSpace(fragment)})
	}
	return out
}

// scanFile dispatches on the file type.
func scanFile(f sourceFile) []finding {
	switch filepath.Ext(f.path) {
	case ".html":
		return scanHTML(f.path, f.src)
	case ".css":
		return scanCSSAt(f.path, f.src, 0)
	}
	return scanJS(f.path, f.src)
}

func TestI18nNoHardcodedText(t *testing.T) {
	allow := loadAllowlist(t)
	var bad []string
	for _, f := range sourceFiles(t) {
	next:
		for _, fd := range scanFile(f) {
			for _, a := range allow {
				if a.allows(fd) {
					a.used = true
					continue next
				}
			}
			bad = append(bad, fd.String())
		}
	}
	for _, a := range allow {
		if !a.used {
			t.Logf("allowlist entry no longer needed: %s:%s", a.file, a.fragment)
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d hard-coded UI texts (P14.2: UI text comes from t(); exceptions go to %s):\n%s", len(bad), allowlistPath, strings.Join(bad, "\n"))
	}
}

// ---- plural forms ----

// pluralCategories are the CLDR plural categories that
// Intl.PluralRules(locale) selects from (P14.2): Russian needs one / few /
// many / other, English and Hindi one / other, the Chinese languages other.
// A new language file needs its row here.
var pluralCategories = map[string][]string{
	"zh-CN":       {"other"},
	"zh-TW":       {"other"},
	"yue-Hant-HK": {"other"},
	"en":          {"one", "other"},
	"hi":          {"one", "other"},
	"ru":          {"one", "few", "many", "other"},
}

// pluralProblems lists the plural objects of a catalog that miss a category
// of the locale (t() would fall back to another language for those counts)
// or have a form the locale never selects.
func pluralProblems(locale string, c catalog) []string {
	want, ok := pluralCategories[locale]
	if !ok {
		return []string{fmt.Sprintf("no plural categories known for %s (add them to pluralCategories)", locale)}
	}
	var out []string
	for k := range c.textKeys() {
		obj, ok := c[k].(map[string]any)
		if !ok {
			continue
		}
		var missing, unknown []string
		for _, cat := range want {
			if _, ok := obj[cat]; !ok {
				missing = append(missing, cat)
			}
		}
		for form := range obj {
			if !contains(want, form) {
				unknown = append(unknown, form)
			}
		}
		sort.Strings(unknown)
		if len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s: %s lacks the plural form(s) %s", locale, k, strings.Join(missing, ", ")))
		}
		if len(unknown) > 0 {
			out = append(out, fmt.Sprintf("%s: %s has the form(s) %s, which %s never selects", locale, k, strings.Join(unknown, ", "), locale))
		}
	}
	sort.Strings(out)
	return out
}

func TestI18nPluralForms(t *testing.T) {
	for _, loc := range append([]string{sourceLocale}, otherLocales(t)...) {
		for _, p := range pluralProblems(loc, loadCatalog(t, filepath.Join(catalogDir, loc+".json"))) {
			t.Error(p)
		}
	}
}

// ---- formats come from util.js only (P14.4, lead ruling at the gate) ----

// formatProblems lists what bypasses util.js's formatters in a JavaScript
// file: toLocale*String calls, Intl number / date / relative-time formatters
// built for a locale that is not a fixed literal (that is, the UI
// language), and fmt* imported from i18n.js. A formatter with a fixed
// locale ('en-GB' for comparing wall clocks, say) is not display text.
func formatProblems(file, src string) []string {
	_, toks := lexJS(src)
	var out []string
	add := func(tk jsToken, what string) { out = append(out, fmt.Sprintf("%s:%d: %s", file, tk.line, what)) }
	for i, tk := range toks {
		if tk.kind != 'i' {
			continue
		}
		switch {
		case (tk.val == "toLocaleString" || tk.val == "toLocaleDateString" || tk.val == "toLocaleTimeString") && i > 0 && toks[i-1].val == "." && i+1 < len(toks) && toks[i+1].val == "(":
			add(tk, tk.val+"() (use util.js: fmtInt, fmtDateTime, …)")
		case tk.val == "Intl" && i+3 < len(toks) && toks[i+1].val == "." && toks[i+3].val == "(":
			switch name := toks[i+2].val; name {
			case "NumberFormat", "DateTimeFormat", "RelativeTimeFormat":
				if i+4 < len(toks) && toks[i+4].val != ")" && toks[i+4].kind != 's' {
					add(tk, "Intl."+name+" in the UI language (format through util.js)")
				}
			}
		case tk.val == "toFixed" && i > 0 && toks[i-1].val == "." && i+5 < len(toks) && toks[i+1].val == "(" && toks[i+3].val == ")" && toks[i+4].val == "+" &&
			toks[i+5].kind == 's' && unitRe.MatchString(toks[i+5].val):
			add(tk, "toFixed() + a unit (use util.js: fmtPercent, fmtMB, fmtDecimal, …)")
		case tk.val == "import" && i+1 < len(toks) && toks[i+1].val == "{":
			j := i + 2
			var names []string
			for ; j < len(toks) && toks[j].val != "}"; j++ {
				if toks[j].kind == 'i' && strings.HasPrefix(toks[j].val, "fmt") {
					names = append(names, toks[j].val)
				}
			}
			if j+2 < len(toks) && toks[j+1].val == "from" && strings.HasSuffix(toks[j+2].val, "i18n.js") && len(names) > 0 {
				add(tk, "imports "+strings.Join(names, ", ")+" from i18n.js (format through util.js)")
			}
		}
	}
	// a number written into a template right before a unit: "${n}%",
	// "${(mb / 1024).toFixed(1)} GB" (CSS values are not UI text: the
	// value of width/left/… and of style.setProperty is skipped)
	for i, tk := range toks {
		if tk.kind != 't' || !unitTplRe.MatchString(tk.val) || cssValue(toks, i) {
			continue
		}
		add(tk, "a number glued to a unit in a template (use util.js: fmtPercent, fmtMB, fmtDecimal, …)")
	}
	return out
}

var (
	// a string that starts with a unit: "%", " GB", "°C"
	unitRe = regexp.MustCompile(`^ ?(%|[KMGT]B|kB|°)([^A-Za-z]|$)`)
	// a template whose placeholder is followed by a unit ("{}%", "{} GB")
	unitTplRe = regexp.MustCompile(`[{][}] ?(%|[KMGT]B|kB|°)([^A-Za-z]|$)`)
)

// cssValue: the template at toks[i] is a CSS value — the value of a style
// property (width: …, el.style.left = …) or an argument of setProperty().
func cssValue(toks []jsToken, i int) bool {
	css := map[string]bool{"width": true, "height": true, "left": true, "right": true, "top": true, "bottom": true, "inset": true,
		"minWidth": true, "maxWidth": true, "minHeight": true, "maxHeight": true, "flexBasis": true, "transform": true, "backgroundSize": true, "setProperty": true}
	for j := i - 1; j >= 0 && j >= i-4; j-- {
		if toks[j].kind == 'i' && css[toks[j].val] {
			return true
		}
		if toks[j].kind == 's' && strings.HasPrefix(toks[j].val, "--") { // setProperty('--p', …)
			return true
		}
	}
	return false
}

func TestI18nFormatsOnlyInUtil(t *testing.T) {
	var bad []string
	for _, f := range sourceFiles(t) {
		if filepath.Ext(f.path) != ".js" || f.path == "util.js" || f.path == "i18n.js" {
			continue
		}
		bad = append(bad, formatProblems(f.path, f.src)...)
	}
	if len(bad) > 0 {
		t.Errorf("numbers and dates are formatted by util.js only (P14.4):\n%s", strings.Join(bad, "\n"))
	}
}

// ---- keys passed to t() ----

var tCallRe = regexp.MustCompile("\\bt\\(\\s*(?:'([^'\\\\\\n]*)'|\"([^\"\\\\\\n]*)\"|`([^`$\\\\]*)`)\\s*[,)]")

// nameTableRe: util.js's nameTable('prefix.', ['a', 'b']) — its keys are
// prefix + each name.
var (
	nameTableRe = regexp.MustCompile(`nameTable\(\s*'([^']+)'\s*,\s*\[([^\]]*)\]`)
	quotedRe    = regexp.MustCompile(`'([^']*)'`)
)

func TestI18nKeysExist(t *testing.T) {
	src := loadCatalog(t, filepath.Join(catalogDir, sourceLocale+".json"))
	var missing []string
	for _, f := range sourceFiles(t) {
		if !strings.HasSuffix(f.path, ".js") {
			continue
		}
		stripped, _ := lexJS(f.src)
		for n, ln := range strings.Split(stripped, "\n") {
			for _, m := range tCallRe.FindAllStringSubmatch(ln, -1) {
				key := m[1] + m[2] + m[3]
				if _, ok := src[key]; !ok {
					missing = append(missing, fmt.Sprintf("%s:%d: %s", f.path, n+1, key))
				}
			}
			for _, m := range nameTableRe.FindAllStringSubmatch(ln, -1) {
				for _, q := range quotedRe.FindAllStringSubmatch(m[2], -1) {
					if key := m[1] + q[1]; src[key] == nil {
						missing = append(missing, fmt.Sprintf("%s:%d: %s (name table)", f.path, n+1, key))
					}
				}
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d keys passed to t() are not in %s.json:\n%s", len(missing), sourceLocale, strings.Join(missing, "\n"))
	}
}

// ---- self-tests of the scanner ----

func findingTexts(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.text)
	}
	return out
}

func TestI18nScannerFindsHardcodedText(t *testing.T) {
	js := strings.Join([]string{
		"// 注释里的中文不算",
		"/* Block comment: Some Words */",
		"const a = '中文';",
		"el.textContent = 'Save the file';",
		"btn.title = loading ? 'Loading audio' : 'Play it now';",
		"x.setAttribute('aria-label', 'Close the dialog');",
		"h('span', { class: 'x', title: 'Hello there' }, 'Some child text');",
		"h('span', { 'aria-label': ok ? 'Yes indeed' : 'No way' });",
		"toast('Upload failed badly', 'warn');",
		"const re = /['\"]Not a string/;",
		"const ok = t('common.ok'); h('div', null, t('nav.label'));",
		"h('kbd', { 'aria-keyshortcuts': 'ArrowLeft ArrowRight' });",
		"const fine = `Template ${n ? 'Inner text here' : ''}`;",
		"const ms = 3 / 2; const u = 'Hello world';",
		"el.textContent = `Count ${n} items now`;",
	}, "\n")
	want := []string{
		"中文",
		"Save the file", "Loading audio", "Play it now", "Close the dialog",
		"Hello there", "Some child text", "Yes indeed", "No way", "Upload failed badly",
		"Count {} items now",
	}
	if got := findingTexts(scanJS("x.js", js)); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("scanJS found\n  %q\nwant\n  %q", got, want)
	}
}

// One case per rule added at the gate (fe-fix): tooltips (data-tip,
// dataset.tip), aria-description, aria-valuetext, the first argument of
// openDialog / errorBlock / alert / confirm / prompt, array children of h(),
// templates that start with ${…}, and CJK reported literal by literal.
func TestI18nScannerGateRules(t *testing.T) {
	cases := []struct {
		src, text, why string // why "" = must not be reported
	}{
		{"el.dataset.tip = 'Hover tip text';", "Hover tip text", ".dataset.tip"},
		{"h('span', { dataset: { tip: 'Dataset tip text' } });", "Dataset tip text", "property tip"},
		{"h('span', { 'data-tip': 'Attribute tip text' });", "Attribute tip text", "property data-tip"},
		{"x.setAttribute('data-tip', 'Set tip text');", "Set tip text", "setAttribute('data-tip')"},
		{"h('div', { 'aria-description': 'Describes the chart' });", "Describes the chart", "property aria-description"},
		{"el.ariaDescription = 'Describes the bar';", "Describes the bar", ".ariaDescription"},
		{"bar.ariaValueText = 'Five of ten';", "Five of ten", ".ariaValueText"},
		{"x.setAttribute('aria-valuetext', 'Three of ten');", "Three of ten", "setAttribute('aria-valuetext')"},
		{"h('div', { role: 'slider', 'aria-valuetext': 'Two of ten' });", "Two of ten", "property aria-valuetext"},
		{"openDialog('Dialog title here', build);", "Dialog title here", "openDialog()"},
		{"errorBlock('Could not load', retry);", "Could not load", "errorBlock()"},
		{"alert('Alert text here');", "Alert text here", "alert()"},
		{"if (confirm('Are you sure')) go();", "Are you sure", "confirm()"},
		{"const v = window.prompt('Your name please');", "Your name please", "prompt()"},
		{"h('p', null, ['Array child text', x]);", "Array child text", "array in h() child"},
		{"h('p', null, [a, ok ? ['Nested array text'] : null]);", "Nested array text", "array in array in h() child"},
		{"el.textContent = `${n} files uploaded`;", "{} files uploaded", ".textContent"},
		{"el.title = `${a}: ${b} not ready`;", "{}: {} not ready", ".title"},
		// not UI text
		{"const list = ['Not UI text'];", "", ""},
		{"const v = obj['Some key here'];", "", ""},
		{"el.textContent = `${a} ${b}`;", "", ""},
		{"el.title = `${n}×`;", "", ""},
		{"openDialog(title, (close) => build(close, 'Not the title'));", "", ""},
		{"h('span', { tipText: 'Some other prop' });", "", ""},
	}
	for _, c := range cases {
		got := scanJS("x.js", c.src)
		switch {
		case c.why == "" && len(got) != 0:
			t.Errorf("%s: reported %v", c.src, got)
		case c.why != "" && (len(got) != 1 || got[0].text != c.text || got[0].why != "English UI text in "+c.why):
			t.Errorf("%s: got %v, want %q (English UI text in %s)", c.src, got, c.text, c.why)
		}
	}
	// CJK: every literal on its own, so the allowlist names exactly one
	if got := findingTexts(scanJS("x.js", "const names = ['简体中文', '别的中文']; // 注释")); strings.Join(got, "|") != "简体中文|别的中文" {
		t.Errorf("CJK literals %q", got)
	}
	a := parseAllowlist(t, "x.js:简体中文  # a language name\n")
	if len(a) != 1 || !a[0].allows(finding{file: "x.js", text: "简体中文"}) {
		t.Errorf("exact fragment not allowed: %+v", a)
	} else if a[0].allows(finding{file: "x.js", text: "简体中文和别的中文"}) || a[0].allows(finding{file: "y.js", text: "简体中文"}) {
		t.Errorf("the allowlist matches more than the exact fragment of its file")
	}
}

func TestI18nScannerHTMLAndCSS(t *testing.T) {
	html := strings.Join([]string{
		"<!-- 注释 Some Comment -->",
		"<button aria-label=\"Close this now\">关闭</button>",
		"<p>Some static text</p>",
		"<div data-x='中文属性' aria-description=\"Describes this block\"></div>",
		"<style>.a::after { content: \"Drop it here\"; } /* 中文注释 */</style>",
		"<script>// 中文",
		"var x = '脚本中文';</script>",
		"<noscript><p>需要 JavaScript。 Needs JavaScript.</p></noscript>",
	}, "\n")
	var got []string
	for _, f := range scanHTML("x.html", html) {
		got = append(got, fmt.Sprintf("%d:%s", f.line, f.text))
	}
	want := []string{"7:脚本中文", "5:Drop it here", "2:Close this now", "4:中文属性", "4:Describes this block", "2:关闭", "3:Some static text", "8:需要 JavaScript。 Needs JavaScript."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("scanHTML found\n  %q\nwant\n  %q", got, want)
	}
	css := strings.Join([]string{
		"/* 注释 Comment Words */",
		".a::after { content: \"松开以添加\"; }",
		".b::before { content: 'Drop files here'; }",
		".c::after { content: attr(data-drop); }",
		".d { font-family: \"Segoe UI\", \"微软雅黑\"; }",
		".e { justify-content: center; content: \"·\"; }",
		".f { quotes: \"Some quote text\"; align-content: \"Not content either\"; }",
	}, "\n")
	got = nil
	for _, f := range scanCSSAt("x.css", css, 0) {
		got = append(got, fmt.Sprintf("%d:%s", f.line, f.text))
	}
	want = []string{"2:松开以添加", "3:Drop files here", "5:微软雅黑"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("scanCSSAt found\n  %q\nwant\n  %q", got, want)
	}
}

func TestI18nPluralChecker(t *testing.T) {
	cases := []struct {
		locale string
		c      catalog
		want   string
	}{
		{"ru", catalog{"a": map[string]any{"one": "{n} x", "other": "{n} xs"}, "b": "plain", "_meta": map[string]any{"locale": "ru"}}, "ru: a lacks the plural form(s) few, many"},
		{"ru", catalog{"a": map[string]any{"one": "1", "few": "2", "many": "5", "other": "1.5"}}, ""},
		{"en", catalog{"a": map[string]any{"other": "{n} xs"}}, "en: a lacks the plural form(s) one"},
		{"hi", catalog{"a": map[string]any{"one": "x", "other": "y"}}, ""},
		{"zh-CN", catalog{"a": map[string]any{"one": "x", "other": "y"}}, "zh-CN: a has the form(s) one, which zh-CN never selects"},
		{"yue-Hant-HK", catalog{"a": map[string]any{"other": "y"}}, ""},
		{"ja", catalog{}, "no plural categories known for ja (add them to pluralCategories)"},
	}
	for _, c := range cases {
		if got := strings.Join(pluralProblems(c.locale, c.c), "; "); got != c.want {
			t.Errorf("%s: %q, want %q", c.locale, got, c.want)
		}
	}
}

func TestI18nFormatChecker(t *testing.T) {
	src := strings.Join([]string{
		"import { t, fmtDate } from './i18n.js';",
		"const a = n.toLocaleString('zh-CN');",
		"const f = new Intl.NumberFormat(getLocale(), { notation: 'compact' });",
		"const g = new Intl.DateTimeFormat('en-GB', { timeZone: tz });",
		"const z = Intl.DateTimeFormat().resolvedOptions().timeZone;",
		"const d = new Intl.DisplayNames([loc], { type: 'language' });",
		"// x.toLocaleString() in a comment",
		"import { fmtInt } from './util.js';",
		"const s = `${d.toLocaleDateString()}`;",
		"const gb = (mb / 1024).toFixed(1) + ' GB';",
		"el.style.width = `${(f * 100).toFixed(1)}%`; h('i', { style: { width: `${p * 100}%` } }); root.style.setProperty('--p', `${p}%`);",
		"h('span', null, `${Math.round(o * 100)}%`); const v = `${(mb / 1024).toFixed(1)} GB`;",
		"const n = x.toFixed(2); const w = `${n} files`; const k = `${n} GBP`;",
	}, "\n")
	got := formatProblems("x.js", src)
	var lines []string
	for _, p := range got {
		lines = append(lines, strings.SplitN(p, ":", 3)[1])
	}
	if strings.Join(lines, ",") != "1,2,3,9,10,12,12" {
		t.Errorf("formatProblems:\n%s", strings.Join(got, "\n"))
	}
}

func TestI18nKeyRegexp(t *testing.T) {
	src := "t('a.b'); t(\"c.d\", {n}); t(`e.f`); t(`g.${x}`); t('h.' + x); split('x'); it('no')"
	var keys []string
	for _, m := range tCallRe.FindAllStringSubmatch(src, -1) {
		keys = append(keys, m[1]+m[2]+m[3])
	}
	if strings.Join(keys, ",") != "a.b,c.d,e.f" {
		t.Errorf("keys %v", keys)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
