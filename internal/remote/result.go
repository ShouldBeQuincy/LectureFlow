// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

// Checking a remote's result.json (plan §5: the remote is not fully
// trusted). ValidateResult takes the bytes the controller downloaded and
// returns the result re-encoded with only the keys LectureFlow reads, or an
// error: then the whole result is refused (CodeResultInvalid, the lecture
// goes back to the local devices). The checks: size, UTF-8, structure
// (types, depth), at most MaxResultSentences sentences of at most
// MaxSentenceRunes characters, control characters removed, start times
// non-decreasing and inside the audio, word times inside their sentence;
// unknown keys dropped.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits of a result.
const (
	MaxResultSentences    = 50000
	MaxSentenceRunes      = 4000
	MaxWordsPerSentence   = 4000
	MaxLowConfPerSentence = 4000
	MaxWordRunes          = 200
	MaxResultParts        = 1000
	// ResultTimeSlackMS: how far past the audio's end (and past a
	// sentence's ends, for its words) a time may be.
	ResultTimeSlackMS = 2000
	wordSlackMS       = 50
	maxTimingDepth    = 4
	maxTimingKeys     = 256
)

// ErrResultInvalid is wrapped by every refusal.
var ErrResultInvalid = errors.New("remote result refused")

func refuse(why string) error { return fmt.Errorf("%w: %s", ErrResultInvalid, why) }

type rawResult struct {
	PromptTermsUsed json.RawMessage `json:"prompt_terms_used"`
	Sentences       json.RawMessage `json:"sentences"`
	Timing          json.RawMessage `json:"timing"`
	Parts           json.RawMessage `json:"parts"`
}

type rawSentence struct {
	StartMS      *json.Number    `json:"start_ms"`
	EndMS        *json.Number    `json:"end_ms"`
	Text         *string         `json:"text"`
	LowConfWords json.RawMessage `json:"low_conf_words"`
	Words        json.RawMessage `json:"words"`
}

// ResultSentence is one checked sentence.
type ResultSentence struct {
	StartMS      int64        `json:"start_ms"`
	EndMS        int64        `json:"end_ms"`
	Text         string       `json:"text"`
	LowConfWords []ResultLow  `json:"low_conf_words"`
	Words        []ResultWord `json:"words,omitempty"`
}

// ResultLow is a low-confidence word (model.LowConfWord).
type ResultLow struct {
	I int    `json:"i"`
	W string `json:"w"`
}

// ResultWord is a word timestamp (model.Word).
type ResultWord struct {
	W string `json:"w"`
	S int64  `json:"s"`
	E int64  `json:"e"`
}

// ResultPart is a part of a resumed run (model.ASRPart).
type ResultPart struct {
	FromMS    int64  `json:"from_ms"`
	ToMS      int64  `json:"to_ms"`
	DeviceID  string `json:"device_id"`
	Model     string `json:"model"`
	Precision string `json:"precision"`
}

// Result is a checked result.json.
type Result struct {
	PromptTermsUsed int              `json:"prompt_terms_used"`
	Sentences       []ResultSentence `json:"sentences"`
	Timing          map[string]any   `json:"timing,omitempty"`
	Parts           []ResultPart     `json:"parts,omitempty"`
}

var partFieldRe = regexp.MustCompile(`^[A-Za-z0-9:._-]{0,64}$`)

// ValidateResult checks raw (a remote's result.json for audio of
// audioMS) and returns the re-encoded result.
func ValidateResult(raw []byte, audioMS int64) ([]byte, *Result, error) {
	if len(raw) == 0 || len(raw) > MaxResultBytes {
		return nil, nil, refuse("size")
	}
	if !utf8.Valid(raw) {
		return nil, nil, refuse("not UTF-8")
	}
	if err := checkDepth(raw, 8); err != nil {
		return nil, nil, err
	}
	var rr rawResult
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, nil, refuse("not a JSON object")
	}
	if !jsonObject(raw) {
		return nil, nil, refuse("not a JSON object")
	}
	var res Result
	if len(rr.PromptTermsUsed) > 0 && string(rr.PromptTermsUsed) != "null" {
		n, err := intOf(rr.PromptTermsUsed, 0, 1_000_000)
		if err != nil {
			return nil, nil, refuse("prompt_terms_used")
		}
		res.PromptTermsUsed = int(n)
	}
	if len(rr.Sentences) == 0 || string(rr.Sentences) == "null" {
		return nil, nil, refuse("no sentences")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(rr.Sentences, &raws); err != nil {
		return nil, nil, refuse("sentences is not an array")
	}
	if len(raws) > MaxResultSentences {
		return nil, nil, refuse("too many sentences")
	}
	if audioMS <= 0 {
		// without the audio's length the times would have no upper bound
		// (SEC41 L4); the asr stage sends no such lecture to a remote
		return nil, nil, refuse("audio length")
	}
	limit := audioMS + ResultTimeSlackMS
	prevStart := int64(-1)
	res.Sentences = make([]ResultSentence, 0, len(raws))
	for i, r := range raws {
		s, err := checkSentence(r, limit)
		if err != nil {
			return nil, nil, refuse(fmt.Sprintf("sentence %d: %s", i, err.Error()))
		}
		if s.StartMS < prevStart {
			return nil, nil, refuse(fmt.Sprintf("sentence %d: start before the previous sentence's", i))
		}
		prevStart = s.StartMS
		res.Sentences = append(res.Sentences, s)
	}
	if len(rr.Timing) > 0 && string(rr.Timing) != "null" {
		t, err := checkTiming(rr.Timing)
		if err != nil {
			return nil, nil, refuse("timing: " + err.Error())
		}
		res.Timing = t
	}
	if len(rr.Parts) > 0 && string(rr.Parts) != "null" {
		var ps []ResultPart
		dec := json.NewDecoder(bytes.NewReader(rr.Parts))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ps); err != nil || len(ps) > MaxResultParts {
			return nil, nil, refuse("parts")
		}
		for _, p := range ps {
			if p.FromMS < 0 || p.ToMS < p.FromMS || p.ToMS > limit || !partFieldRe.MatchString(p.DeviceID) ||
				!partFieldRe.MatchString(p.Model) || !partFieldRe.MatchString(p.Precision) {
				return nil, nil, refuse("parts")
			}
		}
		res.Parts = ps
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, nil, refuse("encode")
	}
	return out, &res, nil
}

func jsonObject(raw []byte) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == '{'
}

// checkDepth refuses JSON nested deeper than max (arrays and objects).
func checkDepth(raw []byte, max int) error {
	depth := 0
	inStr, esc := false, false
	for _, c := range raw {
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
			if depth > max {
				return refuse("nested too deeply")
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}

func intOf(raw json.RawMessage, lo, hi int64) (int64, error) {
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, err
	}
	return numberIn(n, lo, hi)
}

func numberIn(n json.Number, lo, hi int64) (int64, error) {
	i, err := n.Int64()
	if err != nil {
		f, ferr := n.Float64()
		if ferr != nil || f != math.Trunc(f) {
			return 0, errors.New("not an integer")
		}
		i = int64(f)
	}
	if i < lo || i > hi {
		return 0, errors.New("out of range")
	}
	return i, nil
}

func checkSentence(raw json.RawMessage, limit int64) (ResultSentence, error) {
	var s ResultSentence
	var rs rawSentence
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&rs); err != nil || !jsonObject(raw) {
		return s, errors.New("not an object")
	}
	if rs.StartMS == nil || rs.EndMS == nil || rs.Text == nil {
		return s, errors.New("start_ms, end_ms or text missing")
	}
	var err error
	if s.StartMS, err = numberIn(*rs.StartMS, 0, limit); err != nil {
		return s, errors.New("start_ms " + err.Error())
	}
	if s.EndMS, err = numberIn(*rs.EndMS, 0, limit); err != nil {
		return s, errors.New("end_ms " + err.Error())
	}
	if s.EndMS < s.StartMS {
		return s, errors.New("ends before it starts")
	}
	if utf8.RuneCountInString(*rs.Text) > MaxSentenceRunes {
		return s, errors.New("text too long")
	}
	s.Text = cleanControl(*rs.Text)
	s.LowConfWords = []ResultLow{}
	if len(rs.LowConfWords) > 0 && string(rs.LowConfWords) != "null" {
		var lows []struct {
			I *json.Number `json:"i"`
			W *string      `json:"w"`
		}
		d := json.NewDecoder(bytes.NewReader(rs.LowConfWords))
		d.UseNumber()
		if err := d.Decode(&lows); err != nil || len(lows) > MaxLowConfPerSentence {
			return s, errors.New("low_conf_words")
		}
		for _, l := range lows {
			if l.I == nil || l.W == nil || utf8.RuneCountInString(*l.W) > MaxWordRunes {
				return s, errors.New("low_conf_words")
			}
			i, err := numberIn(*l.I, 0, MaxWordsPerSentence)
			if err != nil {
				return s, errors.New("low_conf_words")
			}
			s.LowConfWords = append(s.LowConfWords, ResultLow{I: int(i), W: cleanControl(*l.W)})
		}
	}
	if len(rs.Words) > 0 && string(rs.Words) != "null" {
		var ws []struct {
			W *string      `json:"w"`
			S *json.Number `json:"s"`
			E *json.Number `json:"e"`
		}
		d := json.NewDecoder(bytes.NewReader(rs.Words))
		d.UseNumber()
		if err := d.Decode(&ws); err != nil || len(ws) > MaxWordsPerSentence {
			return s, errors.New("words")
		}
		lo, hi := max(s.StartMS-wordSlackMS, 0), s.EndMS+wordSlackMS
		for _, w := range ws {
			if w.W == nil || w.S == nil || w.E == nil || utf8.RuneCountInString(*w.W) > MaxWordRunes {
				return s, errors.New("words")
			}
			st, err1 := numberIn(*w.S, lo, hi)
			en, err2 := numberIn(*w.E, lo, hi)
			if err1 != nil || err2 != nil || en < st {
				return s, errors.New("word time outside its sentence")
			}
			s.Words = append(s.Words, ResultWord{W: cleanControl(*w.W), S: st, E: en})
		}
	}
	return s, nil
}

// cleanControl drops control and format characters from the text of a
// sentence or a word (tabs and newlines become spaces). ZWNJ (U+200C) and
// ZWJ (U+200D) stay: they are part of the text in Persian, Hindi and other
// scripts and of emoji sequences, which a local transcript keeps too
// (SEC41 L3); the bidi controls (U+061C among them), U+200B, U+2060–U+206F
// and the BOM go.
// Identifier-like strings (CleanText) keep the stricter rule.
func cleanControl(s string) string {
	drop := func(r rune) bool { return r < 0x20 || (r >= 0x7f && r < 0xa0) || isTextFormatRune(r) }
	if !strings.ContainsFunc(s, drop) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case drop(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isTextFormatRune is isFormatRune without the joiners ZWNJ and ZWJ, for
// the text of sentences and words (cleanControl).
func isTextFormatRune(r rune) bool {
	return isFormatRune(r) && r != 0x200c && r != 0x200d
}

// checkTiming keeps the timing block's numbers and booleans (and objects of
// them, a few levels deep); strings only when short and printable.
func checkTiming(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	keys := 0
	out, err := cleanTiming(m, 1, &keys)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func cleanTiming(m map[string]any, depth int, keys *int) (map[string]any, error) {
	if depth > maxTimingDepth {
		return nil, errors.New("nested too deeply")
	}
	out := map[string]any{}
	for k, v := range m {
		*keys++
		if *keys > maxTimingKeys || len(k) > 64 || !partFieldRe.MatchString(k) {
			return nil, errors.New("bad key")
		}
		switch x := v.(type) {
		case json.Number:
			f, err := x.Float64()
			if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, errors.New("bad number")
			}
			out[k] = x
		case bool, nil:
			out[k] = x
		case string:
			if utf8.RuneCountInString(x) > 64 {
				return nil, errors.New("string too long")
			}
			out[k] = CleanText(x, 64)
		case map[string]any:
			c, err := cleanTiming(x, depth+1, keys)
			if err != nil {
				return nil, err
			}
			out[k] = c
		default:
			// arrays are not part of the timing block: dropped
		}
	}
	return out, nil
}
