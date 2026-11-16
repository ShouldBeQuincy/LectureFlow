// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"fmt"
	"strings"
)

// TranslateBatchRequest is one batch translate request (v3.2.x P2, Q14a):
// the consecutive TARGET sentences [first, first+n) in one request,
// answered with {"items": [{"idx", "zh"}]} (SchemaTranslateBatch).
type TranslateBatchRequest struct {
	System string // TranslateBatchSystem: byte-identical for every batch of a lecture
	User   string
	Idxs   []int    // Sentence.Idx of every TARGET, in order (the number in its label)
	Hashes []string // zh_hash of every TARGET: Hashes[k] == TranslateHash(lec, first+k, x)
}

// TranslateBatchSystem is the batch translate prefix: TranslateBatchSystemTemplate
// with the glossary (SPEC-v2 G5.1 sections) and the summary, filled exactly
// like TranslateSystem.
func TranslateBatchSystem(lec LectureSnapshot) string {
	return fill(TranslateBatchSystemTemplate,
		"glossary_lines", GlossaryLines(lec.Terms),
		"summary", summaryOrNone(lec.Summary))
}

// BuildTranslateBatchRequest builds the batch request for the n consecutive
// sentences [first, first+n) with context window x. The user message uses
// the labels of BuildTranslateRequest, one [TARGET #idx] section per target:
//
//	[BEFORE]
//	text of first-x .. first-1, one per line, or (none)
//	[TARGET #first]
//	text of first
//	[ASR]                       only when asr_text differs from text,
//	(ASRNote)                   right after its own target
//	asr_text of first
//	[TARGET #first+1]
//	…
//	[AFTER]
//	text of last+1 .. last+x, one per line, or (none)
//
// For n = 1 the user message equals BuildTranslateRequest(lec, first, x).User
// (only the system prompt differs). The hashes are the per-sentence hashes
// of SPEC §7.3 (TranslateHash), unchanged by batching. first and n must
// describe a non-empty range of valid sentence indexes (panics otherwise).
func BuildTranslateBatchRequest(lec LectureSnapshot, first, n, x int) TranslateBatchRequest {
	ss := lec.Sentences
	if n < 1 || first < 0 || first+n > len(ss) {
		panic(fmt.Sprintf("ctxbuild: batch [%d,%d) out of range [0,%d)", first, first+n, len(ss)))
	}
	x = max(x, 0)
	last := first + n - 1
	lo, hi := max(0, first-x), min(len(ss)-1, last+x)

	var b strings.Builder
	contextSection(&b, LabelBefore, ss[lo:first])
	prefix := translateHashPrefix(lec)
	req := TranslateBatchRequest{
		System: TranslateBatchSystem(lec),
		Idxs:   make([]int, 0, n),
		Hashes: make([]string, 0, n),
	}
	for i := first; i <= last; i++ {
		t := ss[i]
		fmt.Fprintf(&b, LabelTarget+"\n%s\n", t.Idx, oneLine(t.Text))
		if !sameText(t.ASRText, t.Text) {
			b.WriteString(LabelASR + "\n" + ASRNote + "\n")
			b.WriteString(oneLine(t.ASRText))
			b.WriteByte('\n')
		}
		req.Idxs = append(req.Idxs, t.Idx)
		req.Hashes = append(req.Hashes, translateHash(prefix, ss, i, x))
	}
	contextSection(&b, LabelAfter, ss[last+1:hi+1])
	req.User = strings.TrimSuffix(b.String(), "\n")
	return req
}
