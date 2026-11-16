// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

// JSON Schemas of the answers the prompts ask for (v3.2.x P1, Q3a). A
// provider that can enforce a schema (Claude's structured output, llm
// package) gets the one of the request, so the answer is exactly one JSON
// value of that shape. They say what the "Respond with JSON only: {…}" line
// of each prompt says, nothing more: the prompts are unchanged and the
// stages' checks still decide whether an answer is usable.
//
// The schema language of structured output: every object has
// "additionalProperties": false and lists all its properties as required;
// no length, range or array-size constraints. The answers of the glossary
// janitor (an action's fields depend on the action) and of the file name
// rule (kind_map has free keys) do not fit it and have no schema.
const (
	// SchemaTranslate: SPEC §13.5, {"idx": <target idx>, "zh": "..."}.
	SchemaTranslate = `{"type":"object","properties":{"idx":{"type":"integer"},"zh":{"type":"string"}},"required":["idx","zh"],"additionalProperties":false}`

	// SchemaTranslateBatch: the batch translate request (v3.2.x P2,
	// TranslateBatchSystemTemplate), {"items": [{"idx": <target idx>, "zh": "..."}]}.
	// No item-count constraint (structured output has none): the stage
	// checks that every TARGET has exactly one item.
	SchemaTranslateBatch = `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"idx":{"type":"integer"},"zh":{"type":"string"}},"required":["idx","zh"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`

	// SchemaRepair: SPEC §13.4, {"patches": [{"idx", "after", "class", "reason"}]}.
	SchemaRepair = `{"type":"object","properties":{"patches":{"type":"array","items":{"type":"object","properties":{"idx":{"type":"integer"},"after":{"type":"string"},"class":{"type":"string","enum":["auto","absorb","decide"]},"reason":{"type":"string"}},"required":["idx","after","class","reason"],"additionalProperties":false}}},"required":["patches"],"additionalProperties":false}`

	// SchemaTerms: SPEC §13.1 and §13.2 (glossary_seed, material_terms),
	// {"terms": [{"en": "...", "zh": "..."}]}.
	SchemaTerms = `{"type":"object","properties":{"terms":{"type":"array","items":{"type":"object","properties":{"en":{"type":"string"},"zh":{"type":"string"}},"required":["en","zh"],"additionalProperties":false}}},"required":["terms"],"additionalProperties":false}`

	// SchemaChapters: SPEC-v2 P7, {"chapters": [{"start_idx", "title_en", "title_zh"}]}.
	SchemaChapters = `{"type":"object","properties":{"chapters":{"type":"array","items":{"type":"object","properties":{"start_idx":{"type":"integer"},"title_en":{"type":"string"},"title_zh":{"type":"string"}},"required":["start_idx","title_en","title_zh"],"additionalProperties":false}}},"required":["chapters"],"additionalProperties":false}`

	// SchemaTitle: SPEC-v2 P13.5 with the kind of the lecture kinds
	// amendment, {"title_en", "title_zh", "blurb_en", "blurb_zh", "kind"}.
	SchemaTitle = `{"type":"object","properties":{"title_en":{"type":"string"},"title_zh":{"type":"string"},"blurb_en":{"type":"string"},"blurb_zh":{"type":"string"},"kind":{"type":"string"}},"required":["title_en","title_zh","blurb_en","blurb_zh","kind"],"additionalProperties":false}`
)

// AnswerSchemas lists the schemas above by the name of their request.
var AnswerSchemas = map[string]string{
	"translate":       SchemaTranslate,
	"translate_batch": SchemaTranslateBatch,
	"repair":          SchemaRepair,
	"terms":           SchemaTerms,
	"chapters":        SchemaChapters,
	"title":           SchemaTitle,
}

// SecondJSONProblem is the retry feedback for an answer that holds a JSON
// value followed by another, different one (the model corrected itself
// after answering): neither was used.
const SecondJSONProblem = "The answer contained more than one JSON value: a JSON object followed by more text and a second, different JSON object. Decide on the final answer first, then respond with exactly one JSON object and nothing before or after it."
