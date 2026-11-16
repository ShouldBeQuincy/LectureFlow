// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package blocklist

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The extension's list (extension/src/lib/blocklist.js, between the
// blocklist:begin and blocklist:end markers, one entry per line) and this
// one are the same, in the same order: a site added to one is added to the
// other.
func TestSameListAsTheExtension(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "extension", "src", "lib", "blocklist.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	begin, end := strings.Index(src, "// blocklist:begin"), strings.Index(src, "// blocklist:end")
	if begin < 0 || end < begin {
		t.Fatal("blocklist.js has no blocklist:begin / blocklist:end markers")
	}
	line := regexp.MustCompile(`(?m)^\s*\['(youtube|courses)', '([^']+)'\],`)
	var js [][2]string
	for _, m := range line.FindAllStringSubmatch(src[begin:end], -1) {
		js = append(js, [2]string{m[1], m[2]})
	}
	if len(js) < 90 {
		t.Fatalf("read only %d entries from blocklist.js", len(js))
	}
	goList := Entries()
	for i := 0; i < max(len(js), len(goList)); i++ {
		var a, b [2]string
		if i < len(js) {
			a = js[i]
		}
		if i < len(goList) {
			b = goList[i]
		}
		if a != b {
			t.Fatalf("entry %d: blocklist.js has %v, internal/blocklist has %v (keep both lists equal, in the same order)", i, a, b)
		}
	}
}

func TestHosts(t *testing.T) {
	blocked := map[string]string{
		"youtube.com":                  CategoryYouTube,
		"www.youtube.com":              CategoryYouTube,
		"rr3---sn-abc.googlevideo.com": CategoryYouTube,
		"YOUTUBE.COM.":                 CategoryYouTube,
		"www.youtube.com:443":          CategoryYouTube,
		"ｙｏｕｔｕｂｅ。com":                  CategoryYouTube,
		"user:pw@www.udemy.com:8443":   CategoryCourses,
		"mp4-c.udemycdn.com":           CategoryCourses,
		"learning.oreilly.com":         CategoryCourses,
		"www.icourse163.org":           CategoryCourses,
		"practicum.yandex.ru":          CategoryCourses,
		"mitxonline.mit.edu":           CategoryCourses,
		"www.ximalaya.com":             CategoryCourses,
		"higher.smartedu.cn":           CategoryCourses,
		"youtubeeducation.com":         CategoryYouTube, // YouTube's embed host for schools (AUD3 信-4)
		"www.youtubeeducation.com":     CategoryYouTube,
	}
	for h, want := range blocked {
		if got := HostCategory(h); got != want {
			t.Errorf("HostCategory(%q) = %q, want %q", h, got, want)
		}
	}
	for _, h := range []string{"", "notyoutube.com", "youtube.com.example.org", "udemy.com.evil.example", "oreilly.com",
		"www.linkedin.com", "www.bilibili.com", "yandex.ru", "tenant.cloud.panopto.eu", "yuketang.cn", "www.open.edu", "127.0.0.1",
		"mooc1.chaoxing.com", "www.zhihuishu.com", "mit.edu", "ocw.mit.edu", "notyoutubeeducation.com", "youtubeeducation.com.example.org"} {
		if got := HostCategory(h); got != "" {
			t.Errorf("HostCategory(%q) = %q, want none", h, got)
		}
	}
}

func TestURLs(t *testing.T) {
	blocked := map[string]string{
		"https://www.youtube.com/watch?v=x":                    CategoryYouTube,
		"https://rr1---sn-x.googlevideo.com/videoplayback?a=b": CategoryYouTube,
		"https://www.youtubeeducation.com/embed/x":             CategoryYouTube,
		"https://WWW.UDEMY.COM.:443/course/x/":                 CategoryCourses,
		"https://evil.example@udemy.com/":                      CategoryCourses,
		"https://www.linkedin.com/learning":                    CategoryCourses,
		"https://www.linkedin.com/learning/course/intro":       CategoryCourses,
		"https://LinkedIn.com/LEARNING/x":                      CategoryCourses,
		"https://www.linkedin.com//learning/x":                 CategoryCourses,
		"https://www.linkedin.com/%6Cearning/x":                CategoryCourses,
		"https://www.linkedin.com/feed/../learning/x":          CategoryCourses,
		"https://www.linkedin.com/learning;jsessionid=1":       CategoryCourses,
		"https://m.bilibili.com/cheese/play/ep1":               CategoryCourses,
		"blob:https://www.udemy.com/0000":                      CategoryCourses,
		"HTTP://www.coursera.org:8080/learn/x":                 CategoryCourses,
	}
	for u, want := range blocked {
		if got := URLCategory(u); got != want {
			t.Errorf("URLCategory(%q) = %q, want %q", u, got, want)
		}
	}
	for _, u := range []string{"", "not a url", "ftp://www.udemy.com/", "https://www.linkedin.com/feed/",
		"https://www.linkedin.com/learning-login/", "https://www.bilibili.com/video/BV1", "https://www.bilibili.com/cheesecake",
		"http://udemy.com.evil.example/course", "https://lms.example.edu/?u=https://www.udemy.com/", "https://www.oreilly.com/library/",
		"https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1", "https://www.yuketang.cn/v2/web/index", "https://www.open.edu/openlearn/"} {
		if got := URLCategory(u); got != "" {
			t.Errorf("URLCategory(%q) = %q, want none", u, got)
		}
	}
	if got := NormalizePath("/A//B/./c/../D/"); got != "/a/b/d/" {
		t.Errorf("NormalizePath: %q", got)
	}
}

// SEC10 info-2: net/http maps a host by IDNA before dialling (U+00AD SOFT
// HYPHEN and zero-width characters dropped, circled and full-width letters
// made plain), so the list refuses every host that is not plain ASCII
// rather than guess what it maps to. ASCII (punycode) hosts are judged as
// before.
func TestUnusualHosts(t *testing.T) {
	for _, h := range []string{
		"you­tube.com",        // soft hyphen: net/http dials youtube.com
		"ⓨⓞⓤⓣⓤⓑⓔ.com",         // circled letters
		"you​tube.com",        // zero-width space
		"www.ud‍emy.com",      // zero-width joiner
		"www.coursera。org",    // ideographic full stop (folded: still the entry's category)
		"lms.example.edu‮",    // a bidi control
		"bücher.example",      // a Unicode name: its punycode form is what the extension sends
		"lms\x00.example.edu", // control characters
		"lms\t.example.edu",
		"lms .example.edu",
		"lms%2e.example.edu", // a percent sign ("%25" in the URL)
		"lms.example.edu\x7f",
	} {
		if !UnusualHost(h) {
			t.Errorf("UnusualHost(%q) = false", h)
		}
		if got := HostCategory(h); got == "" {
			t.Errorf("HostCategory(%q) = none, want a refusal", h)
		}
	}
	// a match of the list keeps its category; anything else is unusual_host
	for h, want := range map[string]string{
		"you­tube.com":     CategoryUnusualHost,
		"www.coursera。org": CategoryCourses,
		"bücher.example":   CategoryUnusualHost,
	} {
		if got := HostCategory(h); got != want {
			t.Errorf("HostCategory(%q) = %q, want %q", h, got, want)
		}
	}
	for _, h := range []string{"xn--bcher-kva.example", "LMS.Example.EDU.", "user:pw@lms.example.edu:8443", "[fe80::1%25eth0]",
		"fe80::1%eth0", "[2001:db8::1]:443", "127.0.0.1", "media_01.cdn.example", ""} {
		if UnusualHost(h) {
			t.Errorf("UnusualHost(%q) = true", h)
		}
		if got := HostCategory(h); got != "" {
			t.Errorf("HostCategory(%q) = %q, want none", h, got)
		}
	}
	for u, want := range map[string]string{
		"https://you­tube.com/watch?v=x":                       CategoryUnusualHost,
		"https://you%C2%ADtube.com/watch?v=x":                  CategoryUnusualHost, // percent-encoded: url.Parse decodes it
		"https://www.%E2%93%A8outube.com/":                     CategoryUnusualHost, // circled y
		"https://www.you%25tube.com/":                          CategoryUnusualHost,
		"https://www.linked​in.com/learning/x":                 CategoryUnusualHost,
		"https://www.linked​in.com/feed/":                      CategoryUnusualHost, // refused whatever the path
		"https://lms.b%C3%BCcher.example/v":                    CategoryUnusualHost,
		"blob:https://you­tube.com/0000":                       CategoryUnusualHost,
		"https://xn--bcher-kva.example/v":                      "",
		"https://lms.example.edu/?u=https://you%C2%ADtube.com": "",
	} {
		if got := URLCategory(u); got != want {
			t.Errorf("URLCategory(%q) = %q, want %q", u, got, want)
		}
	}
}

// SEC11 low-1: a name is letters, digits, "-", "_" and single dots (one at
// the end at most); any other ASCII character or an empty label is unusual
// too. IP addresses keep their rules; a port and user info are set aside;
// a match of the list keeps its category.
func TestUnusualHostCharset(t *testing.T) {
	for _, h := range []string{
		"a;b.evil.example", "127.0.0.1;.evil.example", "localhost;", "127.0.0.1,", "*.localhost", "a*b",
		"media!cdn.example", "media$cdn.example", "media&cdn.example", "media'cdn.example", "media(cdn.example",
		"media)cdn.example", "media+cdn.example", "media,cdn.example", "media=cdn.example", "media~cdn.example",
		"media<cdn.example", "media>cdn.example", `media"cdn.example`, "media`cdn.example", "media{cdn}.example",
		"media|cdn.example", "media^cdn.example", "media#cdn.example", "media/cdn.example", "media?cdn.example",
		`media\cdn.example`, "localhost..", "foo..bar", ".example.com", "..", ".", "a.b..", "printer.local..",
		"127.0.0.1..", "0x7f.1..", "a;b.example:8443", "user:pw@a;b.example:443", "foo..bar:80",
		"evil.example:x;y@a,b.example", "127.0.0.1%x", // a zone on an IPv4 address
	} {
		if !UnusualHost(h) {
			t.Errorf("UnusualHost(%q) = false", h)
		}
		if got := HostCategory(h); got != CategoryUnusualHost {
			t.Errorf("HostCategory(%q) = %q, want %q", h, got, CategoryUnusualHost)
		}
	}
	for _, h := range []string{
		"a_b.example", "media-01.cdn_x.example", "example.com.", "LOCALHOST", "localhost.", "0x7f.1", "0x7f.1.",
		"2130706433", "127.1", "127.0.0.1.", "xn--bcher-kva.example", "lms.example.edu:8443", "lms.example.edu:",
		"user:pw;x@lms.example.edu", "::1", "[::1]", "[::1]:443", "fe80::1%eth0", "[fe80::1%25eth0]",
		"fe80::1%a;b", "::ffff:127.0.0.1", "192.0.2.7", "192.0.2.7:80",
	} {
		if UnusualHost(h) {
			t.Errorf("UnusualHost(%q) = true", h)
		}
	}
	// the list first: a listed site with an empty label keeps its category
	for h, want := range map[string]string{
		"youtube.com..": CategoryYouTube, ".youtube.com": CategoryYouTube, "a..youtube.com": CategoryYouTube,
		"www.udemy.com.:443": CategoryCourses, "a;b.udemy.com": CategoryCourses,
	} {
		if got := HostCategory(h); got != want {
			t.Errorf("HostCategory(%q) = %q, want %q", h, got, want)
		}
	}
	for u, want := range map[string]string{
		"https://a;b.evil.example/v.mp4":          CategoryUnusualHost,
		"https://localhost../v.mp4":               CategoryUnusualHost,
		"https://www.linkedin.com../learning/x":   CategoryUnusualHost, // (a path entry: the host alone is unusual)
		"https://youtube.com../watch":             CategoryYouTube,
		"https://media-01.cdn_x.example/v.mp4":    "",
		"https://lms.example.edu./v.mp4":          "",
		"blob:https://a;b.evil.example/0000":      CategoryUnusualHost,
		"https://lms.example.edu/?u=http://a;b.c": "",
	} {
		if got := URLCategory(u); got != want {
			t.Errorf("URLCategory(%q) = %q, want %q", u, got, want)
		}
	}
}
