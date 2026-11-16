// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"lectureflow/internal/db"
)

// walUserVersion reads the schema version (user_version) a database's
// write-ahead log at walPath gives it: the one in the newest committed
// frame of page 1, as SQLite itself reads it (D15: a newer LectureFlow
// stopped hard after its migrations leaves the new version only there;
// db.HeaderUserVersion sees the file header alone). The log is only read.
// Only frames SQLite would use count: the salts of the WAL header, valid
// checksums (https://www.sqlite.org/fileformat.html, 4.1-4.3), a page
// number other than 0, up to the last commit frame. ok is false when the log is missing, damaged, holds
// no committed page 1, or its version does not look like one (as
// HeaderUserVersion: 0 to db.SchemaVersion() + db.HeaderVersionHeadroom).
func walUserVersion(walPath string) (version int, ok bool) {
	f, err := os.Open(walPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return walVersionOf(f)
}

// walVersionOf is walUserVersion reading the log from f.
func walVersionOf(f io.Reader) (version int, ok bool) {
	r := bufio.NewReaderSize(f, 1<<16)
	var hdr [32]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, false
	}
	magic := binary.BigEndian.Uint32(hdr[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 || binary.BigEndian.Uint32(hdr[4:8]) != 3007000 {
		return 0, false
	}
	var order binary.ByteOrder = binary.LittleEndian
	if magic&1 == 1 {
		order = binary.BigEndian
	}
	ps := binary.BigEndian.Uint32(hdr[8:12])
	if ps < 512 || ps > 65536 || ps&(ps-1) != 0 {
		return 0, false
	}
	s0, s1 := walChecksum(order, 0, 0, hdr[:24])
	if s0 != binary.BigEndian.Uint32(hdr[24:28]) || s1 != binary.BigEndian.Uint32(hdr[28:32]) {
		return 0, false
	}
	salt := hdr[16:24]
	frame := make([]byte, 24+int(ps))
	pending, pendingOK := 0, false // page 1 in the frames read so far
	for {
		if _, err := io.ReadFull(r, frame); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return 0, false
		}
		if string(frame[8:16]) != string(salt) {
			break
		}
		if binary.BigEndian.Uint32(frame[0:4]) == 0 {
			// SQLite takes no frame of page 0, nor any after it (SEC31
			// L1: frames of zeros can chain valid checksums, and a sparse
			// file of them would be read to its end)
			break
		}
		s0, s1 = walChecksum(order, s0, s1, frame[:8])
		s0, s1 = walChecksum(order, s0, s1, frame[24:])
		if s0 != binary.BigEndian.Uint32(frame[16:20]) || s1 != binary.BigEndian.Uint32(frame[20:24]) {
			break
		}
		if binary.BigEndian.Uint32(frame[0:4]) == 1 {
			page := frame[24:]
			if string(page[:16]) != "SQLite format 3\x00" {
				return 0, false
			}
			pending, pendingOK = int(int32(binary.BigEndian.Uint32(page[60:64]))), true
		}
		if binary.BigEndian.Uint32(frame[4:8]) != 0 && pendingOK { // a commit frame
			version, ok = pending, true
		}
	}
	if ok && (version < 0 || version > db.SchemaVersion()+db.HeaderVersionHeadroom) {
		return 0, false
	}
	return version, ok
}

// walChecksum continues the WAL checksum s0, s1 over b (a multiple of 8
// bytes), its 32-bit words read in order.
func walChecksum(order binary.ByteOrder, s0, s1 uint32, b []byte) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += order.Uint32(b[i:]) + s1
		s1 += order.Uint32(b[i+4:]) + s0
	}
	return s0, s1
}
