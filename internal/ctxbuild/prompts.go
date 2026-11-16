// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import "strings"

// System prompts of SPEC §13, verbatim, with the amendments of SPEC-v2 G5
// (translate rules 1 and 2). Only the {...} placeholders are replaced
// (single pass, see fill). prompts_test.go and prompts_v2_test.go compare
// every constant against the code blocks of SPEC.md and SPEC-v2.md (their
// prompt sections are in testdata/prompt-spec).

// GlossarySeedSystem is SPEC §13.1 (no placeholders).
const GlossarySeedSystem = `You extract technical terminology from a university course syllabus. The terms will be used to bias speech recognition of lecture recordings and to keep English-to-Chinese translation consistent.

Include: technical terms, named algorithms, protocols, standards, tools, languages, acronyms, and proper nouns specific to the subject.
Exclude: generic academic or administrative words (lecture, assessment, coursework, week, module, learning outcome), and common English words.

For each term give:
- "en": the term exactly as it should be spelled in English.
- "zh": the standard Simplified Chinese translation used in Chinese university textbooks. Use "" for acronyms, product names, and proper nouns that Chinese speakers normally keep in English.

Return at most 150 terms, most subject-specific first.
Respond with JSON only: {"terms": [{"en": "...", "zh": "..."}]}`

// GlossarySeedUserTemplate is the §13.1 user message.
const GlossarySeedUserTemplate = "SYLLABUS:\n{syllabus_text}"

// SPEC §13.2: same as §13.1 with the first sentence and the limit changed.
const (
	glossarySeedFirstSentence  = "You extract technical terminology from a university course syllabus."
	materialTermsFirstSentence = "You extract technical terminology from lecture slides of a university course."
	glossarySeedLimit          = "at most 150"
	materialTermsLimit         = "at most 100"
)

// MaterialTermsSystem is SPEC §13.2, derived from GlossarySeedSystem.
var MaterialTermsSystem = strings.Replace(
	strings.Replace(GlossarySeedSystem, glossarySeedFirstSentence, materialTermsFirstSentence, 1),
	glossarySeedLimit, materialTermsLimit, 1)

// MaterialTermsUserTemplate is the §13.2 user message.
const MaterialTermsUserTemplate = "SLIDES:\n{chunk_text}"

// SummarySystemTemplate is SPEC §13.3.
const SummarySystemTemplate = `You summarise a university lecture from its automatic speech-recognition transcript and slides. The transcript contains recognition errors; use the slides and glossary to interpret it correctly.

Write an English summary of at most 800 words covering: the main topic, the key concepts in the order they are taught, definitions given, and any examples or worked problems. Use the correct technical spelling of every term. Plain prose, no headings, no bullet points.

GLOSSARY:
{glossary_lines}

SLIDES:
{slides_text}`

// SummaryUserTemplate is the §13.3 user message.
const SummaryUserTemplate = "TRANSCRIPT:\n{asr_sentences_joined_by_newline}"

// RepairSystemTemplate is SPEC §13.4.
const RepairSystemTemplate = `You correct speech-recognition errors in a university lecture transcript. You are given the course glossary, a summary of the lecture, and the lecture slides as reference.

Each input line is "[idx] sentence". Words the recogniser was unsure about are marked like ⟦this⟧.

Rules:
- Only fix recognition errors and obvious disfluencies. Never paraphrase, summarise, or improve the lecturer's style.
- Never merge, split, reorder, add, or remove sentences.
- Only output sentences you change. Output the full corrected sentence in "after", without the ⟦ ⟧ marks.
- Only output idx values from the TARGET section. The CONTEXT section is for reference only.
- If unsure whether something is an error, prefer "decide" over guessing silently.

Classify each change:
- "auto": a clear homophone or near-homophone recognition error, and the corrected word appears in the glossary or the slides.
- "absorb": a minor issue (repeated words, filler, small grammar slip, trivial misrecognition) that would not change the meaning of a translation.
- "decide": a change that could alter the meaning, where a human should listen to the audio to confirm.

Give a short "reason" in English (under 15 words).

Respond with JSON only: {"patches": [{"idx": 0, "after": "...", "class": "auto", "reason": "..."}]}

GLOSSARY:
{glossary_lines}

SUMMARY:
{summary}

SLIDES:
{slides_text}`

// RepairUserTemplate is the §13.4 user message.
const RepairUserTemplate = `CONTEXT (before):
{overlap_before_lines}

TARGET:
{target_lines}

CONTEXT (after):
{overlap_after_lines}`

// TranslateSystemTemplate is SPEC §13.5 as amended by SPEC-v2: rule 2 is
// the G5.2 text and rule 1 names the BEFORE and AFTER sections (G5.3). The
// first line is unchanged (the fake LLM identifies the role by it).
const TranslateSystemTemplate = `You translate a university lecture from English into Simplified Chinese, one sentence at a time. You are given the course glossary and a summary of the lecture.

Rules:
- Translate only the TARGET sentence. The BEFORE and AFTER sentences are context to help you understand it; do not translate them.
- Terms under MANDATORY TERMS must be translated exactly as given; those with no Chinese translation stay in English. Terms under PREFERRED TERMS use the given translation only when the word is used in its technical sense; otherwise translate naturally.
- Keep numbers, formulas, code, variable names, and file names exactly as written.
- Write natural spoken academic Chinese, as a Chinese lecturer would say it. Do not add explanations.
- The ASR line, if present, is the raw speech-recognition output for the target sentence. It may contain errors; use it only if the target sentence seems incomplete.

Respond with JSON only: {"idx": <target idx>, "zh": "..."}

GLOSSARY:
{glossary_lines}

SUMMARY:
{summary}`

// TranslateBatchSystemTemplate is the system prompt of a batch translate
// request (v3.2.x P2, Q14a: several consecutive TARGET sentences per
// request, translate prompt version 3). It is a variant of
// TranslateSystemTemplate, not in the SPEC: its authoritative text is this
// constant and translate_batch_system.golden. It differs from
// TranslateSystemTemplate in exactly four lines (pinned by a test): the
// first line (the fake LLM tells the two apart by it), rule 1, the ASR
// rule and the "Respond with JSON only" line. Rules 2–4 and the glossary
// and summary sections are the same bytes.
const TranslateBatchSystemTemplate = `You translate a university lecture from English into Simplified Chinese, several consecutive sentences at a time. You are given the course glossary and a summary of the lecture.

Rules:
- Translate every TARGET sentence, each on its own: never merge, split, reorder, skip, or add sentences. The BEFORE and AFTER sentences are context to help you understand them; do not translate them. The TARGET sentences are also context for each other.
- Terms under MANDATORY TERMS must be translated exactly as given; those with no Chinese translation stay in English. Terms under PREFERRED TERMS use the given translation only when the word is used in its technical sense; otherwise translate naturally.
- Keep numbers, formulas, code, variable names, and file names exactly as written.
- Write natural spoken academic Chinese, as a Chinese lecturer would say it. Do not add explanations.
- An ASR line, if present, is the raw speech-recognition output for the TARGET sentence just above it. It may contain errors; use it only if that target sentence seems incomplete.

Respond with JSON only, with exactly one item per TARGET sentence: {"items": [{"idx": <target idx>, "zh": "..."}]}

GLOSSARY:
{glossary_lines}

SUMMARY:
{summary}`

// translateSystemTemplateV1 is SPEC.md §13.5 verbatim, FROZEN: it is never
// edited again. The translate hashes (TranslateHash, TranslateStageHash)
// fill it with GlossaryHashText and the summary instead of using the live
// prompt, so a prompt change (G5.5: a new prompt version) never makes a
// stage stale, and an all-enforce glossary keeps every v1 hash (lead
// ruling 9, NOTES.md).
const translateSystemTemplateV1 = `You translate a university lecture from English into Simplified Chinese, one sentence at a time. You are given the course glossary and a summary of the lecture.

Rules:
- Translate only the TARGET sentence. The sentences before and after are context to help you understand it; do not translate them.
- When a glossary term with a Chinese translation appears, you must use that translation. Glossary terms with no Chinese translation stay in English.
- Keep numbers, formulas, code, variable names, and file names exactly as written.
- Write natural spoken academic Chinese, as a Chinese lecturer would say it. Do not add explanations.
- The ASR line, if present, is the raw speech-recognition output for the target sentence. It may contain errors; use it only if the target sentence seems incomplete.

Respond with JSON only: {"idx": <target idx>, "zh": "..."}

GLOSSARY:
{glossary_lines}

SUMMARY:
{summary}`

// Section labels of the translate user message (SPEC §7.2 as amended by
// SPEC-v2 G5.3). LabelTarget takes the sentence idx.
const (
	LabelBefore = "[BEFORE]"
	LabelTarget = "[TARGET #%d]"
	LabelASR    = "[ASR]"
	LabelAfter  = "[AFTER]"
)

// SyllabusFormatSystem is SPEC-v1.1 A8.3 (no placeholders: "{Course code}"
// and "{Course title}" are instructions to the model and stay as written).
const SyllabusFormatSystem = `You reformat a university course syllabus into clean Markdown. Keep every factual detail; do not invent anything. If a section has no information, omit it.

Use exactly these top-level headings, in this order:
# {Course code} {Course title}
## Course Information        (lecturer, term, schedule, credits — whatever is present)
## Learning Outcomes
## Weekly Topics             (a table: Week | Topic | Notes, when weeks are identifiable; otherwise a list)
## Assessment
## Reading List
## Key Terms                 (technical terms that appear in the syllabus, comma-separated)

Output only the Markdown, no explanations.`

// SyllabusFormatUserTemplate is the A8.3 user message.
const SyllabusFormatUserTemplate = "RAW SYLLABUS:\n{raw_text}"

// TranslateRetrySuffix is appended to the translate user message when QA
// rejected the previous translation (SPEC §13.5).
const TranslateRetrySuffix = "\n\nYour previous translation was rejected: {qa_error}. Translate again."

// None is written for an empty placeholder value (SPEC §13: glossary, repair
// context; SPEC-v2 G5.1: an empty glossary section, G5.3: empty translate
// context; NOTES.md: also empty summary / slides).
const None = "(none)"

// fill replaces {name} placeholders in one pass, so placeholder-like text
// inside the inserted values is never expanded again.
func fill(tmpl string, kv ...string) string {
	pairs := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, "{"+kv[i]+"}", kv[i+1])
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// FirstLine returns the first line of a system prompt; the fake LLM server
// identifies the role by it (SPEC §14).
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
