// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/bridge"
	"lectureflow/internal/calendar"
)

// Native fuzz tests of what the extension sends (go test -fuzz=FuzzXxx
// ./internal/fetch). Under a plain go test they run their seeds only.

// refDomainMatch is RFC 6265 5.1.3 for a cookie of the browser: a host-only
// cookie matches its host exactly; a domain cookie also every subdomain of
// it, but an IP address only exactly.
func refDomainMatch(host string, c bridge.Cookie) bool {
	d := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
	host = strings.ToLower(host)
	if d == "" {
		return false
	}
	if host == d {
		return true
	}
	return !c.HostOnly && net.ParseIP(host) == nil && strings.HasSuffix(host, "."+d)
}

// refPathMatch is RFC 6265 5.1.4.
func refPathMatch(reqPath, cookiePath string) bool {
	if reqPath == "" || reqPath[0] != '/' {
		reqPath = "/"
	}
	if cookiePath == "" {
		cookiePath = "/"
	}
	if cookiePath[0] != '/' {
		return false // not a browser's cookie: never sent
	}
	switch {
	case reqPath == cookiePath:
		return true
	case strings.HasPrefix(reqPath, cookiePath) && strings.HasSuffix(cookiePath, "/"):
		return true
	case strings.HasPrefix(reqPath, cookiePath) && reqPath[len(cookiePath)] == '/':
		return true
	}
	return false
}

// FuzzCookieHeader: a request carries exactly the cookies a browser would
// send to its URL (domain, path, Secure, not expired), and the header
// never breaks out of its line.
func FuzzCookieHeader(f *testing.F) {
	f.Add("https://media.example.edu/sessions/a.mp4", "sid", "v", ".example.edu", "/sessions", int64(0), uint8(0))
	f.Add("http://media.example.edu/x", "sid", "v", "media.example.edu", "/", int64(1), uint8(3))
	f.Add("https://127.0.0.1/x", "sid", "v", "0.0.1", "/", int64(0), uint8(0))
	f.Add("https://[::1]:8/x", "sid", "v", "::1", "", int64(0), uint8(2))
	f.Add("https://EXAMPLE.edu./x", "sid", "v", "Example.EDU", "/x/", int64(0), uint8(0))
	f.Add("https://notexample.edu/", "a", "b", "example.edu", "/", int64(0), uint8(0))
	f.Add("https://x.edu/sessX", "a", "b", "x.edu", "/sess", int64(0), uint8(0))
	f.Add("https://u@x.edu@evil.com/", "a", "b", "x.edu", "/", int64(0), uint8(0))
	f.Fuzz(func(t *testing.T, raw, name, value, domain, cpath string, expires int64, flags uint8) {
		now := time.Unix(1_800_000_000, 0)
		c := bridge.Cookie{Name: name, Value: value, Domain: domain, Path: cpath, Expires: expires,
			Secure: flags&1 != 0, HostOnly: flags&2 != 0}
		if !validCookie(c) || name == "zz" {
			return // refused at enqueue (or the other cookie's name)
		}
		other := bridge.Cookie{Name: "zz", Value: "1", Domain: "unrelated.invalid", Path: "/"}
		h := cookieHeader(raw, []bridge.Cookie{c, other}, now)
		if hasCTL(h) {
			t.Fatalf("header %q", h)
		}
		u, err := url.Parse(raw)
		sent, sentOther := false, false
		for _, p := range strings.Split(h, "; ") {
			sent = sent || p == name+"="+value
			sentOther = sentOther || p == "zz=1"
		}
		want := err == nil && !cookieExpired(c, now) && refDomainMatch(u.Hostname(), c) &&
			refPathMatch(u.EscapedPath(), c.Path) && (!c.Secure || u.Scheme == "https")
		if sent != want {
			t.Fatalf("cookie %+v to %q: sent %v, want %v (header %q)", c, raw, sent, want, h)
		}
		if sentOther != (err == nil && refDomainMatch(u.Hostname(), other) && refPathMatch(u.EscapedPath(), "/")) {
			t.Fatalf("the unrelated cookie to %q: sent %v (header %q)", raw, sentOther, h)
		}
	})
}

// FuzzRedactText: a message keeps no query, user info or path of a URL in
// it (ruling 3: host and path hash only), whatever the case of its scheme.
func FuzzRedactText(f *testing.F) {
	f.Add("status=403 URI=", "https", "d1.cloudfront.net", "/a/b.mp4", "Signature=x", " tail")
	f.Add("", "HTTPS", "H", "/Path", "q=1", "")
	f.Add("Get \"", "Http", "u:p@h", "/x", "y", "\": EOF")
	f.Fuzz(func(t *testing.T, prefix, scheme, host, path, query, suffix string) {
		const secret = "Q5ECRET7"
		ok := strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
		if !ok || strings.ContainsAny(host+path+query, " \t\r\n\v\f\"'<>\u0085 ") || strings.Contains(prefix+suffix+host+path, secret) {
			return
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		msg := prefix + scheme + "://" + host + path + "_P4THSECRET?" + query + secret + suffix
		out := redactText(msg)
		if strings.Contains(out, secret) || strings.Contains(out, "P4THSECRET") {
			t.Fatalf("redactText(%q) = %q", msg, out)
		}
	})
}

var reservedStem = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[0-9¹²³]|LPT[0-9¹²³])$`)

// FuzzFileStem: a platform title becomes a file name every system accepts:
// no separator or character Windows forbids, no device name (also before
// an extension), no trailing dot or space, at most 121 characters.
func FuzzFileStem(f *testing.F) {
	for _, s := range []string{"Lecture 3: Graphs / Trees", "  CON ", "CON.mp4", "aux .x", "nul..", "COM¹", "lpt0.tar.gz", "...", "",
		"Week 1.", "讲座 第三讲：图论", "a\x00b\u2028c", "\xff\xfe", "CONIN$", strings.Repeat("长", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, title string) {
		s := fileStem(title)
		if s == "" || utf8.RuneCountInString(s) > 121 || strings.TrimRight(s, ". ") != s {
			t.Fatalf("fileStem(%q) = %q", title, s)
		}
		for _, r := range s {
			if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
				t.Fatalf("fileStem(%q) = %q has %q", title, s, r)
			}
		}
		if s == "." || s == ".." {
			t.Fatalf("fileStem(%q) = %q", title, s)
		}
		stem, _, _ := strings.Cut(s, ".")
		if reservedStem.MatchString(strings.TrimRight(stem, " ")) {
			t.Fatalf("fileStem(%q) = %q is a device name", title, s)
		}
		if t2 := cleanTitle(title); utf8.RuneCountInString(t2) > 200 || strings.Contains(t2, "  ") || strings.TrimSpace(t2) != t2 ||
			strings.IndexFunc(t2, unicode.IsControl) >= 0 {
			t.Fatalf("cleanTitle(%q) = %q", title, t2)
		}
		if k := titleKey(title); titleKey(k) != k {
			t.Fatalf("titleKey(%q) = %q is not stable", title, k)
		}
	})
}

// FuzzRecordedAt: the extension's recording time becomes nil or a value
// lecture.recorded_at can read back.
func FuzzRecordedAt(f *testing.F) {
	for _, s := range []string{"2026-10-20T10:00", "2026-10-20", "2026-10-20T10:00:00+01:00", "9999-12-31T23:59:59-23:59",
		"0000-01-01T00:00:00+01:00", "2026-03-29T01:30", "", " ", "yesterday", "2026-10-20T10:00:00.123456789Z"} {
		f.Add(s)
	}
	loc := time.FixedZone("UTC+8", 8*3600)
	f.Fuzz(func(t *testing.T, s string) {
		v := recordedAt(s)
		if v == nil {
			return
		}
		for _, l := range []*time.Location{time.UTC, loc} {
			p, err := calendar.ParseRecordedAt(*v, l)
			if err != nil || calendar.FormatRecordedAt(p) != *v {
				t.Fatalf("recordedAt(%q) = %q, which reads back as %v, %v", s, *v, p, err)
			}
		}
		// an RFC 3339 time keeps the clock of its own offset (tz-fix)
		if r, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil && *v != r.Format(calendar.RecordedAtLayout) {
			t.Fatalf("recordedAt(%q) = %q, not the stated clock", s, *v)
		}
	})
}

// FuzzCheckTracks: the tracks of an enqueue are refused or recorded
// without anything that downloads them: tracks_json keeps the host and
// the path's hash, never the path, the query or user info.
func FuzzCheckTracks(f *testing.F) {
	f.Add(`[{"url":"https://u:p@cdn.example.edu/s/abc/master.m3u8?Signature=x","kind":"video","format":"hls"},{"url":"https://x/a.m4a","kind":"audio","format":"file"}]`)
	f.Add(`[{"url":"https://x/a.mpd","kind":"unknown","format":"dash","selector":"video:-:0"},{"url":"https://x/b","kind":"video","format":"dash"},{"url":"https://x/c","kind":"video","format":"file"}]`)
	f.Add(`[{"url":"HTTP://X/%2e%2e/a?b#c","kind":"audio","format":"file","selector":"\u0000"}]`)
	f.Add(`[]`)
	f.Fuzz(func(t *testing.T, in string) {
		var tracks []bridge.Track
		if json.Unmarshal([]byte(in), &tracks) != nil {
			return
		}
		for i := range tracks { // labels are the user's to see; only the URL is a credential
			tracks[i].Label, tracks[i].Role, tracks[i].Quality, tracks[i].Selector = "label", "role", "q", "video:-:0"
		}
		live, err := checkTracks(tracks)
		if err != nil {
			return
		}
		audio, other := 0, 0
		for _, l := range live {
			if l.rec.Kind == bridge.KindAudio {
				audio++
			} else {
				other++
			}
		}
		if audio > 1 || other > maxOtherTracks || len(live) != len(tracks) {
			t.Fatalf("%d audio, %d other tracks accepted", audio, other)
		}
		js := tracksJSON(live)
		recs := parseRecords(js)
		if len(recs) != len(live) {
			t.Fatalf("tracks_json %s", js)
		}
		for i, tr := range tracks {
			u, err := url.Parse(tr.URL)
			if err != nil || !validHTTPURL(tr.URL) {
				t.Fatalf("accepted URL %q", tr.URL)
			}
			var host string // (as JSON keeps it: an invalid UTF-8 byte of a %-escaped host becomes U+FFFD)
			hb, _ := json.Marshal(u.Host)
			json.Unmarshal(hb, &host)
			if recs[i].Host != host || len(recs[i].PathHash) != 16 || recs[i].Path != "" {
				t.Fatalf("record %+v of %q", recs[i], tr.URL)
			}
			for _, secret := range []string{u.RawQuery, u.EscapedPath(), u.User.String(), u.Fragment} {
				if len(secret) >= 6 && !strings.Contains(u.Host, secret) && strings.Contains(js, secret) {
					t.Fatalf("tracks_json %s keeps %q of %q", js, secret, tr.URL)
				}
			}
		}
	})
}
