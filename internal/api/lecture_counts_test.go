// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// perf-fix: the lists' batch counts (db.Counter) give exactly what the
// per-lecture queries gave.

// noCounter hides db.Counter: the server falls back to the per-lecture
// methods (the implementation before perf-fix).
type noCounter struct{ db.Store }

// ListJobHistory keeps db.JobHistory (Lecture.usage_total, v3.2.x r7): only
// the batch counts are hidden, so the single-lecture views stay comparable.
func (n noCounter) ListJobHistory(ctx context.Context, scope, scopeID string) ([]model.Job, error) {
	return n.Store.(db.JobHistory).ListJobHistory(ctx, scope, scopeID)
}

// countFixtures builds lectures covering every case of the counts and
// returns them by name.
func countFixtures(t *testing.T, e *env) (map[string]Lecture, []Course) {
	t.Helper()
	ctx := context.Background()
	a, b := e.course("A"), e.course("B")
	ls := map[string]Lecture{}
	for _, x := range []struct{ name, course string }{
		{"words", a.ID}, {"empty", a.ID}, {"firstWords", a.ID}, {"gap", a.ID}, {"one", a.ID}, {"plain", b.ID},
	} {
		ls[x.name] = e.lecture(x.course, x.name)
	}
	w := []model.Word{{W: "cache", S: 0, E: 400}}
	sent := func(idx int, words []model.Word, zh string) model.Sentence {
		return model.Sentence{Idx: idx, StartMS: int64(idx) * 1000, EndMS: int64(idx)*1000 + 900, ASRText: "s", RepairedText: "s", ZhStatus: zh, Words: words}
	}
	seed := map[string][]model.Sentence{
		"words":      {sent(0, w, model.ZhOK), sent(1, w, model.ZhQAFailed), sent(2, w, model.ZhStale), sent(3, w, model.ZhOK)},
		"firstWords": {sent(0, w, model.ZhNone), sent(1, nil, model.ZhOK)},
		"gap":        {sent(0, nil, model.ZhOK), sent(1, nil, model.ZhOK), sent(3, w, model.ZhNone)}, // idx 2 missing: no "last"
		"one":        {sent(0, nil, model.ZhNone)},
		"plain":      {sent(0, nil, model.ZhStale), sent(1, nil, model.ZhStale)},
	}
	for name, ss := range seed {
		id := ls[name].ID
		if err := e.st.Tx(ctx, func(tx db.Store) error {
			if err := tx.InsertSentences(ctx, id, ss); err != nil {
				return err
			}
			return tx.RecomputeText(ctx, id, nil)
		}); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(lecture string, idx int, review string) model.Patch {
		return model.Patch{ID: db.NewID(), LectureID: ls[lecture].ID, SentenceIdx: idx, Before: "s", After: "t", Class: "decide",
			Reason: "r", Review: review, CreatedAt: db.Now()}
	}
	if err := e.st.InsertPatches(ctx, []model.Patch{
		patch("words", 0, model.ReviewPending), patch("words", 1, model.ReviewPending), patch("words", 2, model.ReviewAccepted),
		patch("plain", 0, model.ReviewPending), patch("gap", 1, model.ReviewRejected),
	}); err != nil {
		t.Fatal(err)
	}
	for i, en := range []string{"cache", "write-back", "TLB"} {
		if err := e.st.InsertTerm(ctx, model.Term{ID: db.NewID(), CourseID: a.ID, En: en, Zh: "术语" + en, Source: "syllabus", Enabled: i != 1}); err != nil {
			t.Fatal(err)
		}
	}
	return ls, []Course{a, b}
}

func TestLectureCountsBatchEqualsPerLecture(t *testing.T) {
	e := newEnv(t)
	ls, courses := countFixtures(t, e)
	ctx := context.Background()
	c, ok := e.st.(db.Counter)
	if !ok {
		t.Fatal("the SQLite store has no batch counts")
	}
	ids := []string{}
	for _, l := range ls {
		ids = append(ids, l.ID)
	}
	ids = append(ids, "no-such-lecture", ls["words"].ID) // unknown and repeated ids
	batch, err := c.LectureCounts(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	old := &Server{st: noCounter{e.st}}
	for name, l := range ls {
		want, err := old.lectureCountsOne(ctx, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := batch[l.ID]
		// The fallback reads the first sentence only when the last has no
		// words (as before); the batch always has it.
		if want.Last.HasWords {
			got.First = db.SentenceEnd{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: batch %+v, per lecture %+v", name, got, want)
		}
	}
	if z := batch["no-such-lecture"]; z.Sentences != 0 || len(z.Zh) != 4 || z.Last.Found {
		t.Errorf("unknown lecture %+v", z)
	}
	if g := batch[ls["gap"].ID]; g.Sentences != 3 || g.Last.Found || !g.First.Found {
		t.Errorf("gap %+v", g)
	}
	if w := batch[ls["words"].ID]; w.PatchesPending != 2 || w.Zh[model.ZhOK] != 2 || w.Zh[model.ZhQAFailed] != 1 || w.Zh[model.ZhStale] != 1 || !w.Last.HasWords {
		t.Errorf("words %+v", w)
	}

	// Pending patches and term counts.
	var lecs []model.Lecture
	for _, l := range ls {
		lecs = append(lecs, model.Lecture{ID: l.ID})
	}
	got, err := e.srv.pendingPatchCounts(ctx, lecs)
	if err != nil {
		t.Fatal(err)
	}
	want, err := old.pendingPatchCounts(ctx, lecs)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lecs {
		if got[l.ID] != want[l.ID] {
			t.Errorf("pending of %s: %d, want %d", l.ID, got[l.ID], want[l.ID])
		}
	}
	cids := []string{courses[0].ID, courses[1].ID, "no-such-course"}
	gotT, err := e.srv.termCounts(ctx, cids)
	if err != nil {
		t.Fatal(err)
	}
	wantT, err := old.termCounts(ctx, cids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cids {
		if gotT[id] != wantT[id] {
			t.Errorf("terms of %s: %d, want %d", id, gotT[id], wantT[id])
		}
	}
	if gotT[courses[0].ID] != 3 {
		t.Errorf("terms %v", gotT)
	}
}

// The views built with the batch counts equal those built lecture by
// lecture, for every endpoint that uses them.
func TestListViewsBatchEqualsPerLecture(t *testing.T) {
	e := newEnv(t)
	ls, courses := countFixtures(t, e)
	old := newServer(Deps{Store: noCounter{e.st}, Sched: e.sc, Cfg: e.cfg, DataDir: e.cfg.DataDir, Hub: NewHub(), Settings: e.set},
		func(s *Server) {
			s.runCmd = fakeCmd
			s.freeSpace = func(string) (int64, error) { return 100 << 30, nil }
		})
	t.Cleanup(old.diag.waitStartup)
	oldTS := httptest.NewServer(old.Handler())
	t.Cleanup(oldTS.Close)

	get := func(base, path string) any {
		t.Helper()
		res, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
		var v any
		if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if m, ok := v.(map[string]any); ok {
			delete(m, "generated_at")
		}
		return v
	}
	paths := []string{"/api/dashboard", "/api/courses"}
	for _, c := range courses {
		paths = append(paths, "/api/courses/"+c.ID, "/api/courses/"+c.ID+"/lectures")
	}
	for _, l := range ls {
		paths = append(paths, "/api/lectures/"+l.ID)
	}
	for _, p := range paths {
		got, want := get(e.ts.URL, p), get(oldTS.URL, p)
		if !reflect.DeepEqual(got, want) {
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			t.Errorf("%s differs:\nbatch      %s\nper lecture %s", p, gb, wb)
		}
	}
	// The cases show up in the views as before.
	var lv Lecture
	e.call("GET", "/api/lectures/"+ls["firstWords"].ID, nil, http.StatusOK, &lv)
	if !lv.HasWords || lv.Counts.Sentences != 2 || lv.Counts.ZhOK != 1 {
		t.Fatalf("firstWords %+v %+v", lv.Counts, lv.HasWords)
	}
	e.call("GET", "/api/lectures/"+ls["gap"].ID, nil, http.StatusOK, &lv)
	if lv.HasWords || lv.Counts.Sentences != 3 {
		t.Fatalf("gap %+v %+v", lv.Counts, lv.HasWords)
	}
}

// A sentence whose words do not decode fails the view only when the view
// reads it, with the batch as before.
func TestLectureCountsBadWords(t *testing.T) {
	e := newEnv(t)
	ls, _ := countFixtures(t, e)
	ctx := context.Background()
	raw, err := db.OpenDB(filepath.Join(e.dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// "words": its last sentence (idx 3) is always read → error. "gap": no
	// last sentence (idx 2 is missing), so its first (idx 0) is read → error.
	if _, err := raw.ExecContext(ctx, `UPDATE sentence SET words = 'not json' WHERE lecture_id = ? AND idx = 3`, ls["words"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE sentence SET words = 'not json' WHERE lecture_id = ? AND idx = 0`, ls["gap"].ID); err != nil {
		t.Fatal(err)
	}
	old := &Server{st: noCounter{e.st}}
	batch, err := e.st.(db.Counter).LectureCounts(ctx, []string{ls["words"].ID, ls["gap"].ID, ls["one"].ID})
	if err != nil {
		t.Fatalf("one bad row failed the whole batch: %v", err)
	}
	for _, name := range []string{"words", "gap"} {
		want, err := old.lectureCountsOne(ctx, ls[name].ID)
		if err != nil {
			t.Fatal(err)
		}
		got := batch[ls[name].ID]
		lastBad, firstBad := got.Last.Err != nil, got.First.Err != nil
		if lastBad != (want.Last.Err != nil) || (!lastBad && !got.Last.HasWords && firstBad != (want.First.Err != nil)) {
			t.Errorf("%s: batch last %v first %v, per lecture last %v first %v", name, got.Last.Err, got.First.Err, want.Last.Err, want.First.Err)
		}
	}
	for _, name := range []string{"words", "gap"} {
		res, b := e.do("GET", "/api/lectures/"+ls[name].ID, nil)
		if res.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s: %d %s, want 500 (as before)", name, res.StatusCode, b)
		}
	}
	if g := batch[ls["one"].ID]; g.Last.Err != nil || !g.Last.Found {
		t.Errorf("one %+v", g)
	}
}
