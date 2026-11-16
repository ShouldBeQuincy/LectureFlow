// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// HeadSize is how much of a file SniffFile reads.
const HeadSize = 4096

// playlistSigns are the first bytes (after a UTF-8 byte order mark and
// white space, ASCII case ignored) of text files that name other files or
// network sources: what ffmpeg's hls, concat and sdp demuxers probe for, and
// the other playlist and manifest formats (PLS, DASH/SMIL/ASX/XSPF XML)
// that are no recording either. An AMR file starts with "#!AMR": not here.
var playlistSigns = []struct {
	prefix string
	kind   string
}{
	{"#extm3u", "m3u8"},
	{"ffconcat", "ffconcat"},
	{"[playlist]", "pls"},
	{"<?xml", "xml"},
	{"<mpd", "mpd"},
	{"<smil", "smil"},
	{"<asx", "asx"},
	{"<playlist", "xspf"},
	{"v=0\n", "sdp"},
	{"v=0\r\n", "sdp"},
}

// SniffPlaylist returns the kind of text playlist head starts with ("m3u8",
// "ffconcat", "pls", "xml", "mpd", "smil", "asx", "xspf", "sdp"), or "".
func SniffPlaylist(head []byte) string {
	b := bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	b = bytes.TrimLeft(b, " \t\r\n\f\v")
	for _, s := range playlistSigns {
		if len(b) >= len(s.prefix) && bytes.EqualFold(b[:len(s.prefix)], []byte(s.prefix)) {
			return s.kind
		}
	}
	return ""
}

// SniffFile is SniffPlaylist of the first HeadSize bytes of the file at path.
func SniffFile(path string) (string, error) {
	head, err := readHead(path)
	if err != nil {
		return "", err
	}
	return SniffPlaylist(head), nil
}

// readHead returns the first HeadSize bytes of the file at path (fewer for
// a shorter file, none for an empty one).
func readHead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, HeadSize)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:n], nil
}

// htmlSigns are the first bytes (after a UTF-8 byte order mark and white
// space, ASCII case ignored) of an HTML page: what a "recording" saved from
// a link that needed a login, or from an error page, starts with
// (test-media I6). Conservative on purpose: only a document type or the
// html element, each followed by a delimiter, so no media container
// matches (they start with binary signatures).
var htmlSigns = []string{"<!doctype html", "<html"}

// SniffHTML reports whether head starts like an HTML page (htmlSigns),
// also after an XML declaration (an XHTML page): the declaration, then only
// white space and comments, then an HTML sign. An XML playlist or manifest
// (XSPF, DASH, SMIL) has its own root element there, so it stays a playlist.
func SniffHTML(head []byte) bool {
	b := bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	b = bytes.TrimLeft(b, " \t\r\n\f\v")
	if len(b) >= 5 && bytes.EqualFold(b[:5], []byte("<?xml")) {
		if b = skipXMLProlog(b); b == nil {
			return false
		}
	}
	for _, s := range htmlSigns {
		if len(b) <= len(s) || !bytes.EqualFold(b[:len(s)], []byte(s)) {
			continue
		}
		switch b[len(s)] {
		case '>', ' ', '\t', '\r', '\n', '\f':
			return true
		}
	}
	return false
}

// skipXMLProlog returns b after its XML declaration ("<?xml ... ?>") and
// the white space and comments that follow it, or nil when the declaration
// or a comment is not closed within b.
func skipXMLProlog(b []byte) []byte {
	end := bytes.Index(b, []byte("?>"))
	if end < 0 {
		return nil
	}
	b = b[end+2:]
	for {
		b = bytes.TrimLeft(b, " \t\r\n\f\v")
		if !bytes.HasPrefix(b, []byte("<!--")) {
			return b
		}
		end := bytes.Index(b[4:], []byte("-->"))
		if end < 0 {
			return nil
		}
		b = b[4+end+3:]
	}
}
