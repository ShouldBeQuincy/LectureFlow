// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// test-media I6: a login or error page saved as the recording is
// recognised by its first bytes; nothing that merely mentions html is.
func TestSniffHTML(t *testing.T) {
	yes := []string{
		"<!DOCTYPE html>\n<html><head><title>Sign in</title>",
		"<!doctype html><html>",
		"<!DOCTYPE HTML PUBLIC \"-//W3C//DTD HTML 4.01//EN\">",
		"<html>\n<body>404</body></html>",
		"<HTML lang=\"en\">",
		"\xef\xbb\xbf  \r\n<!DOCTYPE html>",
		"\n\t<html\n>",
		// XHTML: an XML declaration, white space and comments, then html
		"<?xml version=\"1.0\"?><html>",
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML 1.0 Strict//EN\">",
		"\xef\xbb\xbf<?XML version='1.0'?>\r\n<!-- login -->\n<!-- page -->\n<html xmlns=\"http://www.w3.org/1999/xhtml\">",
	}
	no := []string{
		"",
		"<htmlx>",
		"<!DOCTYPE htmlfoo>",
		"<html", // nothing after the name: not enough to tell
		"<head><title>x</title></head>",
		// XML playlists and manifests stay playlists (SniffPlaylist)
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<playlist version=\"1\" xmlns=\"http://xspf.org/ns/0/\">",
		"<?xml version=\"1.0\"?>\n<MPD xmlns=\"urn:mpeg:dash:schema:mpd:2011\">",
		"<?xml version=\"1.0\"?><smil><body/></smil>",
		"<?xml version=\"1.0\"?>\n<!-- <html> -->\n<smil>",
		"<?xml version=\"1.0\"?><?xml-stylesheet href=\"a.xsl\"?><html>",
		"<?xml version=\"1.0\"?>text<html>",
		"<?xml version=\"1.0\"?><!-- unclosed <html>",
		"<?xml version=\"1.0\" <html>",
		"<?xml version=\"1.0\"?><htmlx>",
		"RIFF\x00\x00\x00\x00WAVEfmt ",
		"\x00\x00\x00\x18ftypmp42<html>",
		"\x1aE\xdf\xa3 <!DOCTYPE html>",
		"ID3\x04\x00<html>",
		"text before <html>",
	}
	for _, s := range yes {
		if !SniffHTML([]byte(s)) {
			t.Errorf("SniffHTML(%q) = false", s)
		}
	}
	for _, s := range no {
		if SniffHTML([]byte(s)) {
			t.Errorf("SniffHTML(%q) = true", s)
		}
	}
}

// Check refuses an empty file and an HTML page before ffprobe runs.
func TestCheckEmptyAndHTML(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	called := false
	run := func(context.Context, string, ...string) (string, string, error) {
		called = true
		return "mov,mp4,m4a,3gp,3g2,mj2\n", "", nil
	}
	for _, tc := range []struct{ name, body, kind string }{
		{"empty.mp4", "", ProblemEmpty},
		{"login.mp4", "<!DOCTYPE html>\n<html><body>Please sign in</body></html>\n", ProblemHTML},
		{"error.m4a", "  <html>\n<head><title>403 Forbidden</title></head>", ProblemHTML},
		{"xhtml.mp4", "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE html>\n<html xmlns=\"http://www.w3.org/1999/xhtml\">", ProblemHTML},
	} {
		pr, err := Check(context.Background(), run, "ffmpeg", write(tc.name, tc.body))
		if err != nil || pr == nil || pr.Kind != tc.kind || pr.Format != "" {
			t.Errorf("%s: %+v %v, want kind %s", tc.name, pr, err, tc.kind)
		}
	}
	// an XML playlist is still a playlist, not a web page
	if pr, err := Check(context.Background(), run, "ffmpeg", write("list.mp4", "<?xml version=\"1.0\"?>\n<playlist version=\"1\" xmlns=\"http://xspf.org/ns/0/\">")); err != nil || pr == nil || pr.Kind != ProblemPlaylist || pr.Format != "xml" {
		t.Errorf("xspf: %+v %v", pr, err)
	}
	if called {
		t.Error("ffprobe ran for an empty or HTML file")
	}
	// a media file that is not HTML still goes to ffprobe and passes
	if pr, err := Check(context.Background(), run, "ffmpeg", write("ok.mp4", "\x00\x00\x00\x18ftypmp42")); pr != nil || err != nil {
		t.Errorf("mp4: %+v %v", pr, err)
	}
}
