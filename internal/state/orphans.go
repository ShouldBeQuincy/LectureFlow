// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"lectureflow/internal/db"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
)

// Directories of deleted lectures (test-chaos 2). A lecture is deleted row
// first, directory second (§4.6), so a file another program held open (a
// player streaming it, a virus scanner, an editor) or a kill between the
// two left its directory behind for good. The delete therefore marks the
// directory first with a tombstone (TombstoneFile, written before the rows
// are deleted: sched.Scheduler.DeleteLecture), and only a directory with a
// tombstone whose lecture the database does not have is ever removed later:
// by the delete's own retries and by SweepLectureDirs at the next start.
// A lecture directory without a tombstone is never removed by them, even
// when the database has no such lecture (lead ruling): the database may be
// an older backup the user restored, and a recording fetched by the browser
// extension may exist only in that directory. The cost is the directory of
// an upload that was killed before its row was written, which stays.

// TombstoneFile is the tombstone in a lecture directory whose lecture is
// being deleted: JSON {"lecture_id", "deleted_at"}.
const TombstoneFile = ".lectureflow-deleted"

type tombstone struct {
	LectureID string `json:"lecture_id"`
	DeletedAt string `json:"deleted_at"`
}

// WriteTombstone writes the tombstone of lecture lectureID into its
// directory dir (synced, so it is on disk before the rows go). An error
// when the directory does not exist or cannot be written: the delete goes
// on, and its directory is then only removed by the delete itself and its
// retries, never by a later start.
func WriteTombstone(dir, lectureID string, at time.Time) error {
	b, err := json.Marshal(tombstone{LectureID: lectureID, DeletedAt: at.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, TombstoneFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// RemoveTombstone removes dir's tombstone (a delete that did not happen; a
// lecture the database has after all).
func RemoveTombstone(dir string) error {
	err := os.Remove(filepath.Join(dir, TombstoneFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Tombstoned reports whether dir holds a tombstone that names lectureID.
func Tombstoned(dir, lectureID string) bool {
	fi, err := os.Lstat(filepath.Join(dir, TombstoneFile))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, TombstoneFile))
	if err != nil {
		return false
	}
	var t tombstone
	return json.Unmarshal(b, &t) == nil && t.LectureID == lectureID
}

// RemoveTombstonedDir removes dir with its tombstone last: when something in
// it cannot be removed (a file still open), the tombstone stays, so a later
// try still knows that the directory may go.
func RemoveTombstonedDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var first error
	for _, e := range entries {
		if e.Name() == TombstoneFile {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return first
	}
	return os.RemoveAll(dir)
}

// IsRecordID reports whether name has the form of the ids db.NewID makes
// for courses and lectures: a ULID in its canonical spelling (26 upper-case
// Crockford base-32 characters). A directory with any other name is never
// taken for a lecture's.
func IsRecordID(name string) bool {
	if len(name) != ulid.EncodedSize || name != strings.ToUpper(name) {
		return false
	}
	_, err := ulid.ParseStrict(name)
	return err == nil
}

// SweepLectureDirs removes the directories of deleted lectures that their
// delete could not remove, and returns how many it removed. It looks only
// at data_dir/courses/{course}/lectures/{id}, and removes such a directory
// only when all of these hold:
//   - courses, {course}, lectures and {id} are plain directories (no link
//     or junction is followed: media.PlainDir — a cloud placeholder
//     directory of a synced folder is one —, and media.PlainPath for the
//     whole way down from data_dir; SEC49b), and the names of {course} and
//     {id} are record ids (IsRecordID);
//   - {id} holds a tombstone that names {id} (Tombstoned);
//   - the database has no lecture {id} (GetLecture says not found; any other
//     error keeps it). A tombstoned lecture the database has (a newer backup
//     restored, a delete whose transaction failed) is kept, and its stale
//     tombstone removed.
//
// Nothing else under data_dir is read or touched; a directory without a
// tombstone stays whatever the database says. One that cannot be removed
// (a file still open) is logged and stays, with its tombstone, for the next
// start. Startup recovery runs it before the scheduler and the HTTP server
// start.
func SweepLectureDirs(ctx context.Context, st db.Store, dataDir string) int {
	entries, err := os.ReadDir(filepath.Dir(model.CourseDir(dataDir, "x")))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("startup: the course directories could not be read; no lecture directory swept", "err", err)
		}
		return 0
	}
	removed := 0
	for _, c := range entries {
		if !media.PlainDir(c.Type()) || !IsRecordID(c.Name()) { // a link or a junction is not followed
			continue
		}
		lecturesDir := filepath.Join(model.CourseDir(dataDir, c.Name()), "lectures")
		if fi, err := os.Lstat(lecturesDir); err != nil || !media.PlainDir(fi.Mode()) {
			continue // none, or a link or a junction: not followed
		}
		lectures, err := os.ReadDir(lecturesDir)
		if err != nil {
			slog.Warn("startup: lecture directories not read", "course", c.Name(), "err", err)
			continue
		}
		for _, l := range lectures {
			if !media.PlainDir(l.Type()) || !IsRecordID(l.Name()) {
				continue
			}
			dir := model.LectureDir(dataDir, c.Name(), l.Name())
			if !media.PlainPath(dataDir, dir) { // courses itself a link, too
				continue
			}
			if !Tombstoned(dir, l.Name()) {
				continue
			}
			switch _, err := st.GetLecture(ctx, l.Name()); {
			case err == nil:
				if err := RemoveTombstone(dir); err != nil {
					slog.Warn("startup: stale tombstone of an existing lecture not removed", "lecture", l.Name(), "err", err)
				} else {
					slog.Info("startup: a lecture marked as deleted exists; its tombstone was removed", "lecture", l.Name())
				}
				continue
			case !errors.Is(err, db.ErrNotFound):
				slog.Warn("startup: lecture not read; its directory is kept", "lecture", l.Name(), "err", err)
				continue
			}
			if err := RemoveTombstonedDir(dir); err != nil {
				slog.Warn("startup: the directory of a deleted lecture could not be removed (a file in it may still be open); tried again at the next start",
					"lecture", l.Name(), "err", err)
				continue
			}
			removed++
			slog.Info("startup: removed the directory of a deleted lecture", "course", c.Name(), "lecture", l.Name())
		}
	}
	return removed
}

// ErrKeepDir: a deleted lecture's directory must stay (RemoveDeletedLectureDir).
var ErrKeepDir = errors.New("the directory is kept")

// RemoveDeletedLectureDir removes the directory dir of lecture lectureID,
// whose rows were deleted, for the delete's retries (sched): only while the
// database has no such lecture and, when tombstoned (its tombstone was
// written), only while dir holds that tombstone. nil: the directory is gone
// (also when it was already); an error wrapping ErrKeepDir: it must stay
// (the lecture exists again, e.g. a restored database, or its tombstone is
// gone) and no further try is made; any other error: try again later.
func RemoveDeletedLectureDir(ctx context.Context, st db.Store, dir, lectureID string, tombstoned bool) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	switch _, err := st.GetLecture(ctx, lectureID); {
	case err == nil:
		return fmt.Errorf("%w: lecture %s exists", ErrKeepDir, lectureID)
	case !errors.Is(err, db.ErrNotFound):
		return err
	}
	if !tombstoned {
		return os.RemoveAll(dir)
	}
	if !Tombstoned(dir, lectureID) {
		return fmt.Errorf("%w: its tombstone is gone", ErrKeepDir)
	}
	return RemoveTombstonedDir(dir)
}
