// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import "testing"

// TestChaptersInputHashPinnedCases extends TestChaptersInputHashPinned
// (review R26 A-7, backlog T2) with the edge cases of the input: an empty
// sentence list (nil and non-nil), a single sentence, text with newlines and
// non-ASCII characters, the same sentences in another order, and a summary
// with surrounding white space (the hash takes the summary as stored; only
// the request trims it).
//
// Every lecture stores this hash with its chapters (model.Chapters.InputHash)
// and the API compares it with the hash of the current input: if one of
// these values changes, the chapters of every existing lecture show
// "章节可能已过时". Change the function, and these values, only on purpose.
func TestChaptersInputHashPinnedCases(t *testing.T) {
	mixed := []string{"Today:\nstream ciphers.", "缓存命中率 — 95 %", "Ünïcödé café ☕"}
	reversed := []string{mixed[2], mixed[1], mixed[0]}
	for _, tc := range []struct {
		name    string
		summary string
		ss      []Sentence
		want    string
	}{
		{"nothing", "", nil, "e1148ed1acbe76502a80a162705872c1e770a4e5efffae2d573373c7928e559e"},
		{"empty list", "", []Sentence{}, "e1148ed1acbe76502a80a162705872c1e770a4e5efffae2d573373c7928e559e"},
		{"summary only", "Caches.", nil, "daa97916697129b8df2a31c8015f3826c81111bebda31683f8b70681c1c2d7f6"},
		{"one sentence", "Caches.", chapterSentences("Welcome back."), "42f67e80d5a476336f3bb94c968cb0c69d674c0844b9e238136284bc26a65b1c"},
		{"one empty sentence", "", chapterSentences(""), "9f57f66125eab3873ad4ec4b0c35c042c2084e0875e1758ac7e6e58f40685725"},
		{"newlines and non-ASCII", "本讲介绍流密码。\nLFSR 是线性的。", chapterSentences(mixed...), "619077408aa9c11681fb938742d179c867f07b140e643cfb1776df8e7ce34a8d"},
		{"same sentences reversed", "本讲介绍流密码。\nLFSR 是线性的。", chapterSentences(reversed...), "2b349b9df47053d3eea97e9f67eb2f7a5c217b1c43e27c118ad34d8f5c56896d"},
		{"summary with white space", " Caches. \n", chapterSentences("Welcome back."), "3e0b3889cff63041d9c791961c9626b7aa6806c64650792fc03b30cff556b321"},
	} {
		if got := ChaptersInputHash(tc.summary, tc.ss); got != tc.want {
			t.Errorf("%s: ChaptersInputHash changed: %s", tc.name, got)
		}
	}
}
