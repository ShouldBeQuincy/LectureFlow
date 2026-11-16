// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// SEC41 L1: the small requests to a runtime (the kernel list, a kernel's
// start, the Contents API's answers to a PUT, mkdir, delete) read at most
// smallAnswerMax bytes of the answer (a few MB; a real answer is a few
// hundred bytes): a runtime that answers one of them with 5 MiB gets
// errAnswerTooLarge — nothing of the answer is used, not a cut copy — instead
// of having up to 512 MB read. Normal answers are read as before. (Only a
// stand-in server here; a real Colab runtime is not available to the tests.)
func TestSmallRuntimeRequestsHaveASmallLimit(t *testing.T) {
	big := 5 << 20
	if big <= smallAnswerMax {
		t.Fatalf("setup: 5 MiB must be over the limit of %d", smallAnswerMax)
	}
	var huge bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !huge {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/kernels"):
				_, _ = w.Write([]byte(`[{"id":"k1"}]`))
			default:
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"name":"x","type":"file"}`))
			}
			return
		}
		// a JSON answer of 5 MiB, written in bounded chunks
		_, _ = w.Write([]byte(`[{"id":"`))
		chunk := []byte(strings.Repeat("m", 1<<16))
		for n := 0; n < big; n += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}]`))
	}))
	defer srv.Close()
	rt := &runtimeClient{hc: srv.Client(), token: "marker", agent: "t"}
	rt.base, _ = url.Parse(srv.URL + "/rt")
	ctx := context.Background()

	if ids, err := rt.listKernels(ctx); err != nil || len(ids) != 1 || ids[0] != "k1" {
		t.Fatalf("a normal kernel list: %v %v", ids, err)
	}
	if err := rt.upload(ctx, "content/lectureflow/a.flac", []byte("audio"), 0, nil); err != nil {
		t.Fatalf("a normal upload answer: %v", err)
	}

	huge = true
	if ids, err := rt.listKernels(ctx); !errors.Is(err, errAnswerTooLarge) || ids != nil {
		t.Errorf("a 5 MiB kernel list: %v %v, want %v", ids, err, errAnswerTooLarge)
	}
	if err := rt.upload(ctx, "content/lectureflow/a.flac", []byte("audio"), 0, nil); !errors.Is(err, errAnswerTooLarge) {
		t.Errorf("a 5 MiB answer to an upload: %v, want %v", err, errAnswerTooLarge)
	}
	if err := rt.mkdir(ctx, "content/lectureflow"); !errors.Is(err, errAnswerTooLarge) {
		t.Errorf("a 5 MiB answer to mkdir: %v, want %v", err, errAnswerTooLarge)
	}
}
