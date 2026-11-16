// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// SEC4 L6: the call log counts an answer's content blocks by type, a string
// from the provider. A type that is not a short identifier is counted as
// "other", and so is every new type once maxBlockTypes are known — in the
// whole message and in its stream alike. The known types are kept as sent.
func TestBlockTypesCleanedAndCapped(t *testing.T) {
	content := []any{
		map[string]any{"type": "thinking", "thinking": "hmm"},
		map[string]any{"type": "redacted_thinking", "data": "x"},
		map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "text", "text": " there"},
		map[string]any{"type": strings.Repeat("a", maxBlockTypeLen+1)},
		map[string]any{"type": "Text"},
		map[string]any{"type": "sk-ant-api03 key=secret"},
	}
	for i := range 40 {
		content = append(content, map[string]any{"type": fmt.Sprintf("kind_%02d", i)})
	}
	msg, err := json.Marshal(map[string]any{"id": "msg_b", "type": "message", "role": "assistant", "content": content,
		"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 5, "output_tokens": 3}})
	if err != nil {
		t.Fatal(err)
	}
	whole := answerMeta(model.ProviderAnthropic, msg).blocks
	sse, err := llmtest.MessageSSE(msg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := readAnthropicStream(bytes.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	streamed := out.meta.blocks
	for name, got := range map[string]map[string]int{"whole message": whole, "stream": streamed} {
		if len(got) > maxBlockTypes+1 {
			t.Errorf("%s: %d keys, want at most %d: %v", name, len(got), maxBlockTypes+1, got)
		}
		total := 0
		for k, n := range got {
			total += n
			if cleanBlockType(k) != k {
				t.Errorf("%s: key %q kept", name, k)
			}
		}
		if total != len(content) {
			t.Errorf("%s: %d blocks counted, want %d", name, total, len(content))
		}
		if got["thinking"] != 1 || got["redacted_thinking"] != 1 || got["text"] != 2 {
			t.Errorf("%s: known types %v", name, got)
		}
		// 3 types that are not identifiers, and the 40 kinds past the 16th key
		if want := 3 + 40 - (maxBlockTypes - 4); got[BlockTypeOther] != want {
			t.Errorf("%s: other %d, want %d: %v", name, got[BlockTypeOther], want, got)
		}
	}
	if !maps.Equal(whole, streamed) {
		t.Errorf("whole message %v, stream %v", whole, streamed)
	}
}
