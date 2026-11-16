// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/db"
)

// A write-ahead log taken apart and put together again with valid
// checksums (https://www.sqlite.org/fileformat.html, 4.1-4.3), to make
// the smallest changes to a log real SQLite wrote (SEC31).
type testWAL struct {
	bigEndian    bool
	pageSize     uint32
	ckpt         uint32
	salt         [8]byte
	frames       []testFrame
	badHeaderSum bool // the header's checksum written wrong
}

type testFrame struct {
	pgno, commit uint32
	salt         *[8]byte // nil: the header's
	page         []byte
}

func testWALChecksum(be bool, s0, s1 uint32, b []byte) (uint32, uint32) {
	var o binary.ByteOrder = binary.LittleEndian
	if be {
		o = binary.BigEndian
	}
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += o.Uint32(b[i:]) + s1
		s1 += o.Uint32(b[i+4:]) + s0
	}
	return s0, s1
}

func parseTestWAL(t *testing.T, b []byte) testWAL {
	t.Helper()
	if len(b) < 32 {
		t.Fatalf("a log of %d bytes", len(b))
	}
	w := testWAL{bigEndian: binary.BigEndian.Uint32(b[0:4])&1 == 1, pageSize: binary.BigEndian.Uint32(b[8:12]), ckpt: binary.BigEndian.Uint32(b[12:16])}
	copy(w.salt[:], b[16:24])
	fs := 24 + int(w.pageSize)
	for off := 32; off+fs <= len(b); off += fs {
		f := b[off : off+fs]
		if !bytes.Equal(f[8:16], w.salt[:]) {
			break
		}
		w.frames = append(w.frames, testFrame{pgno: binary.BigEndian.Uint32(f[0:4]), commit: binary.BigEndian.Uint32(f[4:8]), page: append([]byte(nil), f[24:]...)})
	}
	if len(w.frames) == 0 {
		t.Fatal("no frames in the log")
	}
	return w
}

func (w testWAL) build() []byte {
	var h [32]byte
	magic := uint32(0x377f0682)
	if w.bigEndian {
		magic = 0x377f0683
	}
	binary.BigEndian.PutUint32(h[0:], magic)
	binary.BigEndian.PutUint32(h[4:], 3007000)
	binary.BigEndian.PutUint32(h[8:], w.pageSize)
	binary.BigEndian.PutUint32(h[12:], w.ckpt)
	copy(h[16:24], w.salt[:])
	s0, s1 := testWALChecksum(w.bigEndian, 0, 0, h[:24])
	binary.BigEndian.PutUint32(h[24:], s0)
	binary.BigEndian.PutUint32(h[28:], s1)
	if w.badHeaderSum {
		h[27] ^= 0x01
	}
	out := append([]byte(nil), h[:]...)
	for _, f := range w.frames {
		var fh [24]byte
		binary.BigEndian.PutUint32(fh[0:], f.pgno)
		binary.BigEndian.PutUint32(fh[4:], f.commit)
		salt := w.salt
		if f.salt != nil {
			salt = *f.salt
		}
		copy(fh[8:16], salt[:])
		s0, s1 = testWALChecksum(w.bigEndian, s0, s1, fh[:8])
		s0, s1 = testWALChecksum(w.bigEndian, s0, s1, f.page)
		binary.BigEndian.PutUint32(fh[16:], s0)
		binary.BigEndian.PutUint32(fh[20:], s1)
		out = append(out, fh[:]...)
		out = append(out, f.page...)
	}
	return out
}

// page1 is the newest page 1 in the log, with user_version v.
func (w testWAL) page1(t *testing.T, v int) []byte {
	t.Helper()
	var p []byte
	for _, f := range w.frames {
		if f.pgno == 1 {
			p = f.page
		}
	}
	if p == nil {
		t.Fatal("no page 1 in the log")
	}
	p = append([]byte(nil), p...)
	binary.BigEndian.PutUint32(p[60:], uint32(int32(v)))
	return p
}

// with returns a copy of w with frames appended.
func (w testWAL) with(f ...testFrame) testWAL {
	x := w
	x.frames = append(append([]testFrame(nil), w.frames...), f...)
	return x
}

// realWAL has SQLite write a database at schema version known in WAL mode
// and returns the database file and its -wal as a process killed before
// a checkpoint leaves them.
func realWAL(t *testing.T) (dbBytes, walBytes []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "w.db")
	abs, _ := filepath.Abs(path)
	raw, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	for _, q := range []string{"CREATE TABLE a(x)", fmt.Sprintf("PRAGMA user_version = %d", db.SchemaVersion()), "INSERT INTO a VALUES (1)"} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if dbBytes, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if walBytes, err = os.ReadFile(path + "-wal"); err != nil {
		t.Fatal(err)
	}
	return dbBytes, walBytes
}

// sqliteUserVersion is what SQLite reads from the database dbBytes with the
// log walBytes (on copies).
func sqliteUserVersion(t *testing.T, dbBytes, walBytes []byte) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.db")
	if err := os.WriteFile(path, dbBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", walBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(path)
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v int
	if err := d.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func walVersionOfBytes(b []byte) (int, bool) { return walVersionOf(bytes.NewReader(b)) }

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// SEC31 L1: SQLite takes no frame of page 0, nor any frame after one; nor
// does walUserVersion. A committed newer page 1 behind a page-0 frame
// (checksums valid) is not read as the version.
func TestWALUserVersionStopsAtPageZero(t *testing.T) {
	known := db.SchemaVersion()
	dbb, walb := realWAL(t)
	w := parseTestWAL(t, walb)
	forged := w.with(
		testFrame{pgno: 0, commit: 1, page: make([]byte, w.pageSize)},
		testFrame{pgno: 1, commit: 3, page: w.page1(t, known+50)},
	).build()
	if s := sqliteUserVersion(t, dbb, forged); s != known {
		t.Fatalf("SQLite reads %d: the test does not reach the case", s)
	}
	if v, ok := walVersionOfBytes(forged); !ok || v != known {
		t.Fatalf("after a page-0 frame: %d %v, want %d", v, ok, known)
	}
}

// SEC31 L1: a log of one page-1 frame whose checksums end at (0, 0) and
// then nothing but zeros (frames of page 0 whose salts and checksums all
// check out; a sparse file costs no disk) is read only up to the first
// zero frame, not to its end.
func TestWALUserVersionZeroFramesReadBounded(t *testing.T) {
	known := db.SchemaVersion()
	const ps = 4096
	page := make([]byte, ps)
	copy(page, "SQLite format 3\x00")
	binary.BigEndian.PutUint32(page[60:], uint32(known+1))
	var h [32]byte
	binary.BigEndian.PutUint32(h[0:], 0x377f0682)
	binary.BigEndian.PutUint32(h[4:], 3007000)
	binary.BigEndian.PutUint32(h[8:], ps)
	s0, s1 := testWALChecksum(false, 0, 0, h[:24])
	var fh [8]byte
	binary.BigEndian.PutUint32(fh[0:], 1)
	binary.BigEndian.PutUint32(fh[4:], 1)
	s0, s1 = testWALChecksum(false, s0, s1, fh[:])
	s0, s1 = testWALChecksum(false, s0, s1, page[:ps-8])
	// the last two words of the page bring the sums to (0, 0)
	binary.LittleEndian.PutUint32(page[ps-8:], -(s0 + s1))
	binary.LittleEndian.PutUint32(page[ps-4:], -s1)
	b := testWAL{pageSize: ps, frames: []testFrame{{pgno: 1, commit: 1, page: page}}}.build()
	if binary.BigEndian.Uint32(b[32+16:]) != 0 || binary.BigEndian.Uint32(b[32+20:]) != 0 {
		t.Fatal("the checksums do not end at (0, 0): the test does not reach the case")
	}
	const zeros = 8 << 20
	c := &countingReader{r: io.MultiReader(bytes.NewReader(b), io.LimitReader(zeroReader{}, zeros))}
	v, ok := walVersionOf(c)
	if !ok || v != known+1 {
		t.Fatalf("%d %v, want %d", v, ok, known+1)
	}
	if limit := int64(len(b)) + 2*(24+ps) + 1<<16; c.n > limit {
		t.Fatalf("read %d bytes of %d, want at most %d", c.n, int64(len(b))+zeros, limit)
	}
}

// zeroReader reads zeros (bounded by io.LimitReader).
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// SEC31 L3: each check of walUserVersion against the smallest change to a
// log real SQLite wrote; where SQLite can read the result, it reads the
// same version.
func TestWALUserVersionChecks(t *testing.T) {
	known := db.SchemaVersion()
	dbb, walb := realWAL(t)
	w := parseTestWAL(t, walb)
	if v, ok := walVersionOfBytes(w.build()); !ok || v != known {
		t.Fatalf("the log rebuilt: %d %v, want %d", v, ok, known)
	}
	other := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	committedNewer := func(v int) testWAL { return w.with(testFrame{pgno: 1, commit: 3, page: w.page1(t, v)}) }
	badHeader := committedNewer(known + 5)
	badHeader.badHeaderSum = true
	bigEndian := committedNewer(known + 5)
	bigEndian.bigEndian = true
	for _, c := range []struct {
		name   string
		log    testWAL
		want   int
		ok     bool
		sqlite bool // compare with what SQLite reads
	}{
		// frames of another generation of the log (other salts) do not
		// count, even when their checksums chain
		{"other salt", w.with(testFrame{pgno: 1, commit: 3, salt: &other, page: w.page1(t, known+5)}), known, true, true},
		// a newer page 1 not followed by a commit frame was never committed
		{"uncommitted page 1", w.with(testFrame{pgno: 1, page: w.page1(t, known+5)}), known, true, true},
		{"commit, then an uncommitted page 1", w.with(
			testFrame{pgno: 2, commit: 3, page: w.frames[len(w.frames)-1].page},
			testFrame{pgno: 1, page: w.page1(t, known+5)}), known, true, true},
		// a header whose checksum is wrong: SQLite ignores the whole log
		{"header checksum", badHeader, 0, false, false},
		// big-endian checksums (magic 0x377f0683), as other machines write
		{"big-endian", bigEndian, known + 5, true, true},
		// versions no LectureFlow writes (as HeaderUserVersion)
		{"beyond the headroom", committedNewer(known + db.HeaderVersionHeadroom + 1), 0, false, false},
		{"negative", committedNewer(-1), 0, false, false},
		{"at the headroom", committedNewer(known + db.HeaderVersionHeadroom), known + db.HeaderVersionHeadroom, true, false},
	} {
		b := c.log.build()
		v, ok := walVersionOfBytes(b)
		if ok != c.ok || (ok && v != c.want) {
			t.Errorf("%s: %d %v, want %d %v", c.name, v, ok, c.want, c.ok)
		}
		if c.sqlite {
			if s := sqliteUserVersion(t, dbb, b); s != c.want {
				t.Errorf("%s: SQLite reads %d, the test expects %d", c.name, s, c.want)
			}
		}
	}
}

// SEC31 L3: the -wal of a database whose file header is damaged proves
// nothing: newerOnError does not read it then. With a valid header the
// same -wal makes the database one from a newer version.
func TestNewerOnErrorWALNeedsAValidHeader(t *testing.T) {
	known := db.SchemaVersion()
	dbb, walb := realWAL(t)
	w := parseTestWAL(t, walb)
	newerWAL := w.with(testFrame{pgno: 1, commit: 3, page: w.page1(t, known+5)}).build()
	put := func(damaged bool) string {
		t.Helper()
		dataDir := t.TempDir()
		b := append([]byte(nil), dbb...)
		if damaged {
			for i := 16; i < 100; i++ {
				b[i] = 'A'
			}
		}
		if err := os.WriteFile(dbFile(dataDir), b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dbFile(dataDir)+"-wal", newerWAL, 0o600); err != nil {
			t.Fatal(err)
		}
		return dataDir
	}
	cause := fmt.Errorf("open failed")
	good := put(false)
	if _, ok := db.HeaderUserVersion(dbFile(good)); !ok {
		t.Fatal("no valid header: the test does not reach the case")
	}
	// reading the -wal is reported as the check step (SEC31 L2)
	oldFirst := heartbeatFirst
	heartbeatFirst = 0 // the first line at once: no race with a quick read
	t.Cleanup(func() { heartbeatFirst = oldFirst })
	var out bytes.Buffer
	if nf := newerOnError(good, db.FileInfo{}, cause, &progress{w: &out}); nf == nil || nf.Code != failDataDirNewer || nf.Params["found"] != known+5 {
		t.Fatalf("valid header: %+v", nf)
	}
	if out.String() != StartingPrefix+stepCheck+"\n" {
		t.Fatalf("progress while reading the -wal: %q", out.String())
	}
	bad := put(true)
	if _, ok := db.HeaderUserVersion(dbFile(bad)); ok {
		t.Fatal("the header is still valid: the test does not reach the case")
	}
	if nf := newerOnError(bad, db.FileInfo{}, cause, nil); nf != nil {
		t.Fatalf("damaged header: %+v", nf)
	}
}
