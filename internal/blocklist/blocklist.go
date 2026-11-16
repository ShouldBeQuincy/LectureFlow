// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package blocklist is LectureFlow's own copy of the browser extension's
// do-not-handle list (extension/src/lib/blocklist.js): YouTube and its
// media hosts, MOOCs and online / paid course platforms (user 2026-10-27:
// "Chrome 版本不能刮 YouTube，全部版本不能刮 MOOC / 付费课程平台"). The
// extension keeps such pages and tracks out on its own; the download queue
// refuses them again (internal/fetch: enqueue and refresh by the page
// address and the track URLs, and every URL a task requests later), so a
// changed or foreign extension cannot send them either.
//
// An entry is a host (that host and every subdomain) or a host with a path
// prefix ("linkedin.com/learning": only that path and below it, at a
// segment boundary). Hosts are compared lower-cased, without a port, user
// info or trailing dot, fullwidth letters folded (browsers send IDNA
// hosts, already ASCII); paths with percent escapes decoded, lower-cased,
// "\" read as "/", repeated slashes collapsed and dot segments resolved.
// A host the list cannot compare safely (UnusualHost: a byte outside
// ASCII, a control character, a percent sign, any other character but
// letters, digits, "-", "_" and dots, an empty label) is refused as a whole
// (CategoryUnusualHost) unless it already matches an entry: net/http maps
// such a name by IDNA before dialling (a soft hyphen dropped, a circled
// letter made plain) to a host the comparison here would miss (SEC10).
// TestSameListAsTheExtension keeps the two lists equal, in the same order.
//
// Deliberately not on it (user 2026-10-27): 雨课堂 yuketang.cn, 超星 / 学习通
// chaoxing.com and 智慧树 zhihuishu.com (schools keep their own lecture
// recordings there) and open.edu. Course stores on their own domains
// (Teachable, Thinkific, Kajabi, 小鹅通 with a custom domain) are not caught.
package blocklist

import (
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// The categories of the list. The extension and LectureFlow's texts say
// which one a refused site belongs to.
const (
	CategoryYouTube = "youtube"
	CategoryCourses = "courses"
	// CategoryUnusualHost is not a list entry: a host with characters the
	// list cannot compare safely (UnusualHost), refused whatever it maps
	// to. The extension sends ASCII (punycode) hosts only.
	CategoryUnusualHost = "unusual_host"
)

// entries is the list: {category, "host" or "host/path"}, as
// extension/src/lib/blocklist.js has it.
var entries = [][2]string{
	// YouTube
	{CategoryYouTube, "youtube.com"},            // YouTube, YouTube Music, m.youtube.com
	{CategoryYouTube, "youtu.be"},               // YouTube short links
	{CategoryYouTube, "youtube-nocookie.com"},   // YouTube's privacy-enhanced embeds
	{CategoryYouTube, "youtubekids.com"},        // YouTube Kids
	{CategoryYouTube, "youtube.googleapis.com"}, // YouTube's API and legacy embed host
	{CategoryYouTube, "youtubeeducation.com"},   // YouTube's embed host for schools (YouTube Player for Education)
	{CategoryYouTube, "googlevideo.com"},        // YouTube's media servers
	// Course platforms: international MOOCs and paid / online course sites
	{CategoryCourses, "udemy.com"},               // Udemy: paid course marketplace (and Udemy Business tenants)
	{CategoryCourses, "udemycdn.com"},            // Udemy's own CDN (course video and images)
	{CategoryCourses, "coursera.org"},            // Coursera: MOOCs, including Coursera for Campus
	{CategoryCourses, "edx.org"},                 // edX: MOOCs
	{CategoryCourses, "edx-video.net"},           // edX's course video host
	{CategoryCourses, "udacity.com"},             // Udacity: paid "Nanodegree" courses
	{CategoryCourses, "skillshare.com"},          // Skillshare: paid creative courses
	{CategoryCourses, "linkedin.com/learning"},   // LinkedIn Learning (only /learning; the rest of LinkedIn is not blocked)
	{CategoryCourses, "lynda.com"},               // Lynda.com, now LinkedIn Learning
	{CategoryCourses, "pluralsight.com"},         // Pluralsight: paid tech courses
	{CategoryCourses, "masterclass.com"},         // MasterClass: paid video classes
	{CategoryCourses, "domestika.org"},           // Domestika: paid creative courses
	{CategoryCourses, "futurelearn.com"},         // FutureLearn: MOOCs
	{CategoryCourses, "khanacademy.org"},         // Khan Academy: free online courses
	{CategoryCourses, "datacamp.com"},            // DataCamp: paid data science courses
	{CategoryCourses, "codecademy.com"},          // Codecademy: paid coding courses
	{CategoryCourses, "brilliant.org"},           // Brilliant: paid interactive courses
	{CategoryCourses, "frontendmasters.com"},     // Frontend Masters: paid video courses
	{CategoryCourses, "egghead.io"},              // egghead: paid developer video courses
	{CategoryCourses, "educative.io"},            // Educative: paid developer courses
	{CategoryCourses, "teamtreehouse.com"},       // Treehouse: paid tech courses
	{CategoryCourses, "learning.oreilly.com"},    // O'Reilly online learning (the rest of oreilly.com is not blocked)
	{CategoryCourses, "wondrium.com"},            // Wondrium (The Great Courses): paid video courses
	{CategoryCourses, "thegreatcourses.com"},     // The Great Courses
	{CategoryCourses, "thegreatcoursesplus.com"}, // The Great Courses Plus
	{CategoryCourses, "creativelive.com"},        // CreativeLive: paid creative classes
	{CategoryCourses, "teachable.com"},           // Teachable: hosted course stores
	{CategoryCourses, "thinkific.com"},           // Thinkific: hosted course stores
	{CategoryCourses, "kajabi.com"},              // Kajabi: hosted course stores
	{CategoryCourses, "mykajabi.com"},            // Kajabi stores' default domain
	{CategoryCourses, "podia.com"},               // Podia: hosted course stores
	{CategoryCourses, "learnworlds.com"},         // LearnWorlds: hosted course stores
	{CategoryCourses, "zerotomastery.io"},        // Zero To Mastery: paid tech courses
	{CategoryCourses, "scrimba.com"},             // Scrimba: paid coding courses
	{CategoryCourses, "cbtnuggets.com"},          // CBT Nuggets: paid IT training
	{CategoryCourses, "acloudguru.com"},          // A Cloud Guru: paid cloud training
	{CategoryCourses, "cloudacademy.com"},        // Cloud Academy: paid cloud training
	{CategoryCourses, "laracasts.com"},           // Laracasts: paid developer screencasts
	{CategoryCourses, "percipio.com"},            // Skillsoft Percipio: company e-learning
	{CategoryCourses, "alison.com"},              // Alison: free online courses
	{CategoryCourses, "kadenze.com"},             // Kadenze: arts and technology MOOCs
	{CategoryCourses, "maven.com"},               // Maven: paid cohort-based courses
	{CategoryCourses, "fun-mooc.fr"},             // FUN (France Université Numérique): French MOOCs
	{CategoryCourses, "miriadax.net"},            // MiríadaX: Spanish-language MOOCs
	{CategoryCourses, "edraak.org"},              // Edraak: Arabic MOOCs
	{CategoryCourses, "kmooc.kr"},                // K-MOOC: Korean national MOOCs
	{CategoryCourses, "gacco.org"},               // gacco: Japanese MOOCs
	{CategoryCourses, "schoo.jp"},                // Schoo: Japanese paid online classes
	{CategoryCourses, "mooc.fi"},                 // MOOC.fi: the University of Helsinki's public MOOCs
	{CategoryCourses, "mitxonline.mit.edu"},      // MITx Online: MIT's MOOCs (the rest of mit.edu is not blocked)
	{CategoryCourses, "openhpi.de"},              // openHPI: the Hasso Plattner Institute's MOOCs
	{CategoryCourses, "openlearning.com"},        // OpenLearning: online course platform
	{CategoryCourses, "gumroad.com"},             // Gumroad: marketplace that sells online courses among other things
	{CategoryCourses, "skool.com"},               // Skool: paid community and course platform
	// Course platforms: Chinese
	{CategoryCourses, "icourse163.org"},      // 中国大学 MOOC
	{CategoryCourses, "icourses.cn"},         // 爱课程
	{CategoryCourses, "cnmooc.org"},          // 好大学在线
	{CategoryCourses, "xuetangx.com"},        // 学堂在线
	{CategoryCourses, "study.163.com"},       // 网易云课堂 (the rest of 163.com is not blocked)
	{CategoryCourses, "ke.qq.com"},           // 腾讯课堂 (the rest of qq.com is not blocked)
	{CategoryCourses, "time.geekbang.org"},   // 极客时间
	{CategoryCourses, "bilibili.com/cheese"}, // B 站课堂 (only /cheese; the rest of bilibili is not blocked)
	{CategoryCourses, "dedao.cn"},            // 得到
	{CategoryCourses, "igetget.com"},         // 得到 (older domain)
	{CategoryCourses, "xiaoe-tech.com"},      // 小鹅通: hosted course stores
	{CategoryCourses, "xeknow.com"},          // 小鹅通 stores' domain
	{CategoryCourses, "lizhiweike.com"},      // 荔枝微课
	{CategoryCourses, "qianliao.tv"},         // 千聊
	{CategoryCourses, "cctalk.com"},          // CCtalk
	{CategoryCourses, "imooc.com"},           // 慕课网: paid IT courses
	{CategoryCourses, "edu.51cto.com"},       // 51CTO 学堂 (the rest of 51cto.com is not blocked)
	{CategoryCourses, "koolearn.com"},        // 新东方在线
	{CategoryCourses, "smartedu.cn"},         // 国家智慧教育平台 (its higher-education part gathers MOOCs)
	{CategoryCourses, "ximalaya.com"},        // 喜马拉雅: audio platform that sells paid courses
	{CategoryCourses, "hahow.in"},            // Hahow 好學校
	{CategoryCourses, "pressplay.cc"},        // PressPlay Academy
	{CategoryCourses, "ewant.org"},           // ewant 育網
	{CategoryCourses, "sharecourse.net"},     // ShareCourse 學聯網
	// Course platforms: Russian
	{CategoryCourses, "stepik.org"},          // Stepik
	{CategoryCourses, "openedu.ru"},          // Открытое образование
	{CategoryCourses, "lektorium.tv"},        // Лекториум
	{CategoryCourses, "universarium.org"},    // Универсариум
	{CategoryCourses, "skillbox.ru"},         // Skillbox
	{CategoryCourses, "netology.ru"},         // Нетология
	{CategoryCourses, "geekbrains.ru"},       // GeekBrains
	{CategoryCourses, "gb.ru"},               // GeekBrains (current domain)
	{CategoryCourses, "practicum.yandex.ru"}, // Яндекс Практикум (the rest of yandex.ru is not blocked)
	{CategoryCourses, "skillfactory.ru"},     // SkillFactory
	{CategoryCourses, "otus.ru"},             // OTUS
	{CategoryCourses, "foxford.ru"},          // Фоксфорд: paid online school
	{CategoryCourses, "htmlacademy.ru"},      // HTML Academy
	// Course platforms: India
	{CategoryCourses, "swayam.gov.in"},       // SWAYAM: national MOOCs
	{CategoryCourses, "nptel.ac.in"},         // NPTEL: IIT video courses
	{CategoryCourses, "unacademy.com"},       // Unacademy
	{CategoryCourses, "byjus.com"},           // BYJU'S
	{CategoryCourses, "vedantu.com"},         // Vedantu
	{CategoryCourses, "pw.live"},             // Physics Wallah
	{CategoryCourses, "upgrad.com"},          // upGrad
	{CategoryCourses, "simplilearn.com"},     // Simplilearn
	{CategoryCourses, "mygreatlearning.com"}, // Great Learning
	{CategoryCourses, "edureka.co"},          // Edureka
	{CategoryCourses, "scaler.com"},          // Scaler
	{CategoryCourses, "codingninjas.com"},    // Coding Ninjas
}

type entry struct {
	category, host, path string
}

var parsed = func() []entry {
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		host, path, _ := strings.Cut(e[1], "/")
		if path != "" {
			path = "/" + path
		}
		out = append(out, entry{category: e[0], host: host, path: path})
	}
	return out
}()

// Entries is the list as {category, entry} pairs, in order (tests).
func Entries() [][2]string {
	return append([][2]string(nil), entries...)
}

func under(host, base string) bool {
	return host == base || strings.HasSuffix(host, "."+base)
}

// foldRune maps what IDNA would map to ASCII for the characters that
// matter in a host: fullwidth ASCII and the ideographic full stops.
func foldRune(r rune) rune {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFF01 + 0x21
	case r == 0x3002 || r == 0xFF0E || r == 0xFF61:
		return '.'
	}
	return r
}

// hostPart is host without the surrounding spaces, user info, port and
// brackets.
func hostPart(host string) string {
	h := strings.TrimSpace(host)
	if i := strings.LastIndexByte(h, '@'); i >= 0 {
		h = h[i+1:]
	}
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.Trim(h, "[]")
}

// NormalizeHost is a host as the list compares it: lower case, no port,
// user info, brackets or trailing dots, fullwidth letters folded. It is
// meaningful for an ASCII host only; HostCategory and URLCategory refuse
// the others (UnusualHost) when no entry matches.
func NormalizeHost(host string) string {
	h := strings.ToLower(strings.Map(foldRune, hostPart(host)))
	return strings.TrimRight(h, ".")
}

// UnusualHost reports a host name that is not plain ASCII as a URL parser
// leaves it: a byte outside ASCII (a Unicode name, U+00AD SOFT HYPHEN, a
// zero-width or circled character, a full-width letter), an ASCII control
// character or space, or a percent sign (what "%25" decodes to) outside an
// IPv6 zone. net/http maps such a name by IDNA before dialling, so no
// comparison without IDNA can tell which host it reaches; browsers and the
// extension send ASCII (punycode) hosts only, so it is refused rather than
// mapped. host is a URL's Hostname() or Host (a port and user info are
// set aside; a space anywhere counts).
//
// A name (anything but an IP address) is also unusual when it is not
// plainName (SEC11 low-1): any ASCII character but a letter, a digit, "-",
// "_" and the dots between labels ("a;b.example", "localhost,", "a*b"),
// or an empty label ("localhost..", "foo..bar", ".example"; one trailing
// dot, the root, stays allowed). Go's resolver refuses such a name, or
// fails to look it up, where aria2c's resolver may well send it to DNS:
// the address check, which lets a failed look-up through, would judge
// nothing while aria2c reaches whatever the name's DNS answers. No real
// host needs those characters. An IP address keeps its own rules (an IPv6
// one with its zone: fe80::1%eth0), and so do the numbers resolvers read
// as one ("0x7f.1", "2130706433", "127.1": plain names here, refused by
// internal/fetch).
func UnusualHost(host string) bool {
	for i := 0; i < len(host); i++ {
		if c := host[i]; c < 0x21 || c >= 0x7f {
			return true
		}
	}
	h := hostPart(host)
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	return !plainName(h)
}

// plainName: labels of ASCII letters, digits, "-" and "_", none empty,
// separated by single dots, with at most one dot at the end.
func plainName(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// HostCategory is the category of a host blocked as a whole (subdomains
// included; not the path entries), CategoryUnusualHost for a host the
// list cannot compare safely (UnusualHost) and no entry matches, or "".
func HostCategory(host string) string {
	if h := NormalizeHost(host); h != "" {
		for _, e := range parsed {
			if e.path == "" && under(h, e.host) {
				return e.category
			}
		}
	}
	if UnusualHost(host) {
		return CategoryUnusualHost
	}
	return ""
}

// NormalizePath is a URL path as the list compares it.
func NormalizePath(escaped string) string {
	var b strings.Builder
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c == '%' && i+2 < len(escaped) && isHex(escaped[i+1]) && isHex(escaped[i+2]) {
			b.WriteByte(unhex(escaped[i+1])<<4 | unhex(escaped[i+2]))
			i += 2
			continue
		}
		b.WriteByte(c)
	}
	p := strings.ToLower(strings.ReplaceAll(b.String(), "\\", "/"))
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	joined := "/" + strings.Join(out, "/")
	if len(out) > 0 && strings.HasSuffix(p, "/") {
		joined += "/"
	}
	return joined
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func pathCovered(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/") || strings.HasPrefix(path, prefix+";")
}

// URLCategory is the category of a blocked page, frame or media URL (host
// or path entries; a blob: URL by the page that made it;
// CategoryUnusualHost for a host the list cannot compare safely), or "" —
// also for anything that is not an http(s) URL.
func URLCategory(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "blob") {
		return URLCategory(u.Opaque)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return ""
	}
	if c := HostCategory(u.Hostname()); c != "" {
		return c
	}
	host := NormalizeHost(u.Hostname())
	if host == "" {
		return ""
	}
	var path string
	for _, e := range parsed {
		if e.path == "" || !under(host, e.host) {
			continue
		}
		if path == "" {
			path = NormalizePath(u.EscapedPath())
		}
		if pathCovered(path, e.path) {
			return e.category
		}
	}
	return ""
}
