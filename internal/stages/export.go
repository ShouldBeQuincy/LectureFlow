// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/calendar"
	"lectureflow/internal/cite"
	"lectureflow/internal/db"
	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/subs"
	"lectureflow/internal/wav"
)

// exportStage writes the four export files of SPEC §6.6 and the citation
// JSON of SPEC-v1.1 A11.2. The two VTT files are cut into subtitle cues by
// subs.SegmentLecture, exactly like the player (SPEC-v2 P4.6); the citation
// JSON carries the calendar numbers of P13.6.
type exportStage struct{ d Deps }

func (s *exportStage) Name() string                 { return model.StageExport }
func (s *exportStage) Resource() sched.ResourceKind { return sched.ResourceCPU }

// InputHash: export has no input hash (SPEC §4.4 defines none).
func (s *exportStage) InputHash(context.Context, string) (string, error) { return "", nil }

// MissingZh is written for a sentence without a translation.
const MissingZh = "（未翻译）"

// exportPageSize is how many sentences are read per query (SPEC §17.4: stream).
const exportPageSize = 500

// ExportSuffixes are the endings of the export files: the four of SPEC
// §6.6 and the citation JSON (SPEC-v1.1 A11.2), in ExportNames order.
var ExportSuffixes = []string{".bilingual.md", ".zh.vtt", ".bilingual.vtt", ".en.txt", ".cite.json"}

// ExportNames returns the export file names of a lecture, ExportBase plus
// each of ExportSuffixes.
func ExportNames(label, title string) []string {
	base := ExportBase(label, title)
	out := make([]string, len(ExportSuffixes))
	for i, suf := range ExportSuffixes {
		out[i] = base + suf
	}
	return out
}

// ExportBase is the name of a lecture's export files without the ending:
// "{number} {English title}" (SPEC-v2 P13.6: Dashboard, player title bar
// and export file names alike, e.g. "W02L1 Stream Ciphers and LFSRs"),
// made a safe file name. label is ExportLabel's; title is lecture.title.
// The "?" of an unnumbered lecture's "W??-01JB2A" cannot appear in a
// Windows file name and becomes "_" ("W__-01JB2A Title").
func ExportBase(label, title string) string {
	return SafeFileName(strings.TrimSpace(label + " " + title))
}

// ExportLabel is the number in lecture id's export file names: its calendar
// number ("W02L1"), or for an unnumbered lecture the label of the citation
// JSON, "W??-" plus an id prefix unique in the course (calendar.CiteLabels
// over every lecture of numbers, as cite.Build computes it), so unnumbered
// lectures with the same title never share file names. numbers is
// CourseLabels' result; a lecture missing from it counts as unnumbered.
func ExportLabel(numbers map[string]calendar.Numbered, id string) string {
	nums := make([]calendar.Numbered, 0, len(numbers)+1)
	for k, n := range numbers {
		n.ID = k
		nums = append(nums, n)
	}
	if _, ok := numbers[id]; !ok {
		nums = append(nums, calendar.Numbered{ID: id, Label: calendar.Unknown})
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i].ID < nums[j].ID })
	labels := calendar.CiteLabels(nums)
	for i, n := range nums {
		if n.ID == id {
			return labels[i]
		}
	}
	return calendar.Unknown
}

// ExportDownloadName is the name a lecture's export file is downloaded as
// (Content-Disposition): ExportBase of the lecture's current number and
// title plus the file's ending, so the transcript and subtitle files
// written before a new title or a renumbering (or before P13.6) download
// under the name the lecture has now. A citation JSON keeps its name: the
// numbers in its content are those of its export run, and its name, which
// carries the same number, must not claim otherwise. A name with none of
// ExportSuffixes is kept too.
func ExportDownloadName(label, title, name string) string {
	if strings.HasSuffix(name, ".cite.json") {
		return name
	}
	for _, suf := range ExportSuffixes {
		if strings.HasSuffix(name, suf) {
			return ExportBase(label, title) + suf
		}
	}
	return name
}

// SafeFileName turns a lecture title into a file name that is valid on
// Windows and elsewhere: characters <>:"/\|?* and control characters become
// "_", whitespace runs become one space, trailing dots/spaces are removed,
// a reserved device name before the first dot (CON, NUL, COM1…) gets a "_"
// appended to it, the length is capped at 100 characters, and an empty
// result becomes "lecture". Bidirectional control characters are dropped
// (lfmedia.StripBidi; test-media I8): a right-to-left override would make
// the name show another extension than its own.
func SafeFileName(title string) string {
	var b strings.Builder
	for _, r := range lfmedia.StripBidi(title) {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case strings.ContainsRune(`<>:"/\|?*`, r) || unicode.IsControl(r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(s) > 100 {
		s = string([]rune(s)[:100])
	}
	s = strings.TrimRight(s, ". ")
	if s == "" {
		return "lecture"
	}
	cut := len(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		cut = i
	}
	switch strings.ToUpper(s[:cut]) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = s[:cut] + "_" + s[cut:] // the device name is the part before the first dot
	}
	return s
}

// mmss formats a time as mm:ss; minutes keep counting past 59 (75:12).
func mmss(ms int64) string {
	sec := max(ms, 0) / 1000
	return fmt.Sprintf("%02d:%02d", sec/60, sec%60)
}

// vttTime formats hh:mm:ss.mmm.
func vttTime(ms int64) string {
	ms = max(ms, 0)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// cueText makes text safe inside a WebVTT cue: one line, and escaped by
// the WebVTT cue text rules (security review 2, L3): "&" and "<" become
// character references ("&amp;", "&lt;"; a raw "<" starts a cue tag and a
// raw "&" a reference), and so does the ">" of a "-->", which a cue's text
// must not contain. A player shows the references as the characters, so
// "A --> B" and "x < y & z" still read as written.
func cueText(s string) string {
	return vttEscaper.Replace(strings.Join(strings.Fields(s), " "))
}

var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", "-->", "--&gt;")

// mdText makes model output (and human edits) safe in bilingual.md
// (security review 2, L3): a Markdown viewer renders raw HTML and images,
// so a prompt-injected "<img src=…>" or "![](https://…)" would load remote
// content when the file is opened. A "<" that can start an HTML tag,
// comment, declaration or processing instruction (followed by a letter,
// "/", "!" or "?") becomes "&lt;" (a character reference, which every
// Markdown renderer shows as "<"; a backslash escape is not understood by
// all of them), and the "[" of "![" is backslash-escaped ("!\[").
//
// Links (AUD5 low-2): a prompt-injected "[click here](https://…)" would be
// a clickable link whose text says something else than its target. An
// inline link needs "(" right after the "]" of its text, and a link
// reference definition "[label]: https://…" (which also turns any "[label]"
// elsewhere, such as the "**[mm:ss]**" timestamps, into a link) needs ":"
// right after its label, also inside a block quote or list item; so a
// backslash goes between a "]" and a following "(" or ":" ("]\(", "]\:").
// Viewers do not show the backslash; with no definition in the file,
// reference links ("[a][b]", "[a]") stay text as well. Brackets anywhere
// else are written as they are.
//
// Every other character is kept as written: "a < b", "x <= y", "i < 10",
// "<-", "&", "[note]".
func mdText(s string) string {
	if !strings.ContainsAny(s, "<!]") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		var next byte
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch {
		case c == '<' && (next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z' || next == '/' || next == '!' || next == '?'):
			b.WriteString("&lt;")
		case c == '!' && next == '[':
			b.WriteString(`!\[`)
			i++
		case c == ']' && (next == '(' || next == ':'):
			b.WriteString(`]\`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// oneLine collapses line breaks (human edits may contain them).
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// CueSentences converts sentences (in idx order) into the input of
// subs.SegmentLecture (lead ruling 8 of phase 2), shared by the VTT export
// and GET /api/lectures/{id}/cues so that the player and the downloaded
// subtitles are cut identically: the English is sentence.text (never
// asr_text, P4.3), asr_text and words only align the word times, and the
// Chinese is zh when it is not blank and zh_status is not none.
func CueSentences(ss []model.Sentence) []subs.Sentence {
	out := make([]subs.Sentence, 0, len(ss))
	for _, s := range ss {
		zh := ""
		if s.Zh != nil && s.ZhStatus != model.ZhNone {
			zh = strings.TrimSpace(*s.Zh)
		}
		var words []subs.Word
		if len(s.Words) > 0 {
			words = make([]subs.Word, len(s.Words))
			for i, w := range s.Words {
				words[i] = subs.Word{W: w.W, S: w.S, E: w.E}
			}
		}
		out = append(out, subs.Sentence{Idx: s.Idx, StartMS: s.StartMS, EndMS: s.EndMS, Text: s.Text, ASRText: s.ASRText, Words: words, Zh: zh})
	}
	return out
}

// writeVTT writes the cues of zh.vtt and bilingual.vtt (P4.6). A bilingual
// cue is the English line, then the Chinese line; a cue without English (a
// sentence that has only a translation) has the Chinese line only. A cue of
// a sentence without translation shows MissingZh, like the Markdown file.
// zh.vtt merges consecutive cues of one sentence that show the same Chinese
// (subs repeats a chunk over the next cue when that cue gets none, P4.4),
// so an untranslated sentence is one MissingZh cue.
func writeVTT(zhVTT, biVTT *bufio.Writer, cues []subs.Cue) {
	zhText := func(c subs.Cue) string {
		if strings.TrimSpace(c.Zh) == "" {
			return MissingZh
		}
		return cueText(c.Zh)
	}
	for i, c := range cues {
		fmt.Fprintf(biVTT, "%d\n%s --> %s\n", i+1, vttTime(c.StartMS), vttTime(c.EndMS))
		if en := cueText(c.En); en != "" {
			fmt.Fprintf(biVTT, "%s\n", en)
		}
		fmt.Fprintf(biVTT, "%s\n\n", zhText(c))
	}
	n := 0
	for i := 0; i < len(cues); {
		c, text := cues[i], zhText(cues[i])
		end := c.EndMS
		j := i + 1
		for ; j < len(cues); j++ {
			d := cues[j]
			if d.SentenceIdx != c.SentenceIdx || d.StartMS != end || zhText(d) != text {
				break
			}
			end = d.EndMS
		}
		n++
		fmt.Fprintf(zhVTT, "%d\n%s --> %s\n%s\n\n", n, vttTime(c.StartMS), vttTime(end), text)
		i = j
	}
}

// MarkCiteExported records that the user exported citation JSON of the
// course (kv cite_exported:{course} = the time, UTC RFC 3339): from then on
// a change that renumbers lectures asks for confirmation (P13.6, lead
// ruling 4). Only user exports count (lead ruling): the API calls it when a
// lecture's *.cite.json is downloaded and on the course-level export; the
// export stage, which writes cite.json automatically, does not.
func MarkCiteExported(ctx context.Context, st db.Store, courseID string, at time.Time) error {
	return st.SetKV(ctx, model.KVCiteExportPrefix+courseID, at.UTC().Format(time.RFC3339))
}

type exportFile struct {
	final string
	tmp   string // what it is written to (tmpFor)
	f     *os.File
	w     *bufio.Writer
}

func (s *exportStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	l, err := d.Store.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	n, err := d.Store.CountSentences(ctx, lectureID)
	if err != nil {
		return err
	}
	// What the citation JSON needs besides the sentences (A11.3).
	var (
		course  model.Course
		terms   []model.Term
		kinds   []model.LectureKind
		summary string
	)
	err = d.Store.Tx(ctx, func(tx db.Store) error {
		var err error
		if course, err = tx.GetCourse(ctx, l.CourseID); err != nil {
			return err
		}
		if terms, err = tx.ListTerms(ctx, l.CourseID); err != nil {
			return err
		}
		if kinds, err = tx.ListLectureKinds(ctx); err != nil { // the citation JSON's "kinds"
			return err
		}
		switch sm, err := tx.GetSummary(ctx, lectureID); {
		case err == nil:
			summary = sm.Text
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// P13.6: the calendar numbers of every lecture of the course.
	numbers, err := CourseLabels(ctx, d, l.CourseID)
	if err != nil {
		return err
	}
	// commitExport removes the files of export/ that are not this run's:
	// only in a lecture directory of its own (R77b, SEC44 low-2; the ids
	// come from the database, an imported library supplies them), with no
	// link at any folder on its way (SEC49 L1). The scheduler's start check
	// stops such a job before it runs; this is the stage's own guard, before
	// it creates, writes or removes anything.
	if !lfmedia.IDElement(l.CourseID) || !lfmedia.IDElement(l.ID) || !lfmedia.PlainPath(d.DataDir, d.lectureDir(l)) {
		return sched.Permanent(model.Errorf("server.internal", nil,
			errors.New("export: the lecture's directory is not one of its own (its ids, or a link on the way); nothing was written or removed")))
	}
	dir := filepath.Join(d.lectureDir(l), model.ExportDirName)
	// The old export stays as it is until the new one is complete
	// (test-chaos 3): the new files are written to temporary names and put
	// in place together at the end (commitExport).
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// P13.6: "{number} {English title}" (ExportBase).
	names := ExportNames(ExportLabel(numbers, l.ID), l.Title)
	files := make([]*exportFile, len(names))
	closeAll := func() {
		for _, f := range files {
			if f != nil && f.f != nil {
				f.f.Close()
				f.f = nil
			}
		}
	}
	defer func() {
		closeAll()
		for _, f := range files {
			if f != nil {
				os.Remove(f.tmp)
			}
		}
	}()
	for i, name := range names {
		final := filepath.Join(dir, name)
		tmp, err := tmpFor(final)
		if err != nil {
			return inUseCoded(CodeExportFileInUse, err)
		}
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		files[i] = &exportFile{final: final, tmp: tmp, f: f, w: bufio.NewWriter(f)}
	}
	md, zhVTT, biVTT, en, citeW := files[0].w, files[1].w, files[2].w, files[3].w, files[4].w
	zhVTT.WriteString("WEBVTT\n\n")
	biVTT.WriteString("WEBVTT\n\n")

	missing := 0
	written := 0
	all := make([]model.Sentence, 0, n) // for the subtitle cues and the citation JSON
	for off := 0; off < n; off += exportPageSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		ss, err := d.Store.ListSentences(ctx, lectureID, off, exportPageSize)
		if err != nil {
			return err
		}
		for _, x := range ss {
			text := oneLine(x.Text)
			zh := ""
			if x.Zh != nil {
				zh = oneLine(*x.Zh)
			}
			if zh == "" {
				zh = MissingZh
				missing++
			}
			// SPEC §6.6: "**[mm:ss]** English\n\nChinese\n", one paragraph per
			// sentence; the texts without raw HTML, images (mdText, L3) or
			// links (AUD5 low-2)
			fmt.Fprintf(md, "**[%s]** %s\n\n%s\n\n", mmss(x.StartMS), mdText(text), mdText(zh))
			fmt.Fprintf(en, "%s\n", text)
			written++
		}
		all = append(all, ss...)
		progress(float64(written) / float64(max(n, 1)) * 0.9)
	}
	// P4.6: the VTT files are the player's cues (the whole lecture at once,
	// so the inter-sentence rule of P4.3 applies).
	writeVTT(zhVTT, biVTT, subs.SegmentLecture(CueSentences(all)))
	progress(0.95)
	// A11.2: lecture-level citation JSON, one lecture, enabled terms only.
	// duration_ms is the media length, as on the lecture page (audio.wav,
	// which prep converts from the whole original file)
	media, _ := wav.DurationMS(filepath.Join(d.lectureDir(l), model.AudioFile))
	doc := cite.Build(course, kinds, numbers, []cite.LectureInput{{Lecture: l, Summary: summary, Sentences: all, MediaMS: media}}, terms, d.now())
	if err := cite.Encode(citeW, doc); err != nil {
		return err
	}
	for _, f := range files {
		if err := f.w.Flush(); err != nil {
			return err
		}
		if err := f.f.Close(); err != nil {
			return err
		}
		f.f = nil
	}
	if err := commitExport(dir, files); err != nil {
		return inUseCoded(CodeExportFileInUse, err)
	}
	if missing > 0 {
		slog.Warn("export: sentences without translation", "lecture", lectureID, "count", missing)
	}
	d.events().Changed("lecture", lectureID)
	progress(1)
	return nil
}

// commitExport puts the new export files in place of the old export, all or
// nothing (replaceFiles): the files of the previous run are replaced, and
// the other files in export/ (the old names of a renamed or renumbered
// lecture) are removed with them, so export/ holds exactly this run's
// files, as when it was emptied first. A file another program has open
// (an editor, a download in progress) stops the commit before anything
// changed: a *fileInUseError naming it.
func commitExport(dir string, files []*exportFile) error {
	pairs := make([]filePair, len(files))
	keep := make(map[string]bool, len(files))
	for i, f := range files {
		pairs[i] = filePair{tmp: f.tmp, final: f.final}
		keep[filepath.Base(f.final)] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var obsolete []string
	for _, e := range entries {
		name := e.Name()
		// .tmp: this run's files, or what an earlier run left behind
		// (startup recovery removes those)
		if keep[name] || strings.HasSuffix(name, ".tmp") || !e.Type().IsRegular() {
			continue
		}
		obsolete = append(obsolete, filepath.Join(dir, name))
	}
	return replaceFiles(pairs, obsolete)
}
