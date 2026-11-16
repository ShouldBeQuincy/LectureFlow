// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	lfmedia "lectureflow/internal/media"
)

// HLS (ext-brief §6.2): Go reads the playlists with the task's cookies; the
// segments, AES-128 keys and init sections go to aria2c; the media playlist
// is rewritten to the local files and the bundled ffmpeg (which has no TLS)
// joins them offline.

// errPlaylist is a playlist that cannot be used; feature != "" names an
// unsupported feature (CodePlaylistUnsupported), else it is malformed.
type errPlaylist struct {
	feature string
	reason  string
}

func (e *errPlaylist) Error() string {
	if e.feature != "" {
		return "playlist: unsupported " + e.feature
	}
	return "playlist: " + e.reason
}

func badPlaylist(format string, a ...any) error {
	return &errPlaylist{reason: fmt.Sprintf(format, a...)}
}

// Bounds of a download plan (fuzz, NOTES). Relative URIs take the
// playlist's query (inheritQuery), so a long signed query times many
// segments could take gigabytes; a 3-hour recording in 2-second segments
// with 1.5 KB signed URLs plans about 8 MB.
const (
	maxPlanBytes   = 32 << 20        // the URLs of one playlist, or of one Representation of an MPD
	maxHLSSegments = maxDASHSegments // segments of one media playlist
)

type hlsVariant struct {
	uri       string
	bandwidth int64
	pixels    int64
	codecs    string
	audio     string // EXT-X-MEDIA group id of its audio
}

type hlsRendition struct {
	typ, group, uri string
	def             bool
}

type masterPlaylist struct {
	variants   []hlsVariant
	renditions []hlsRendition
}

// playlistLines splits a playlist and checks its header. A lone "\r" ends
// a line too, as it does for the bundled ffmpeg, which reads the local
// playlist: a line must not hide another one from this parser (fuzz,
// NOTES). A NUL, which also ends an ffmpeg line, makes the playlist
// malformed.
func playlistLines(body []byte) ([]string, error) {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if bytes.IndexByte(body, 0) >= 0 {
		return nil, badPlaylist("NUL in the playlist")
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r \t")
	}
	for _, l := range lines {
		if l == "" {
			continue
		}
		if l != "#EXTM3U" {
			return nil, badPlaylist("not an M3U8 playlist")
		}
		return lines, nil
	}
	return nil, badPlaylist("empty playlist")
}

func isMasterPlaylist(body []byte) bool {
	return bytes.Contains(body, []byte("#EXT-X-STREAM-INF"))
}

// attrs parses an attribute list (KEY=VALUE,KEY="quoted, value",...).
func attrs(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				val, s = s[1:], ""
			} else {
				val, s = s[1:1+end], s[2+end:]
			}
			if c := strings.IndexByte(s, ','); c >= 0 {
				s = s[c+1:]
			} else {
				s = ""
			}
		} else if c := strings.IndexByte(s, ','); c >= 0 {
			val, s = s[:c], s[c+1:]
		} else {
			val, s = s, ""
		}
		out[strings.ToUpper(key)] = strings.TrimSpace(val)
	}
	return out
}

func tagValue(line, tag string) (string, bool) {
	if !strings.HasPrefix(line, tag) {
		return "", false
	}
	rest := line[len(tag):]
	if rest == "" {
		return "", true
	}
	if rest[0] != ':' {
		return "", false
	}
	return rest[1:], true
}

// resolve makes ref absolute against base; only http(s) results with a
// host are usable ("http:" alone resolves to no host; fuzz).
// A URI without a query of its own on the playlist's own host takes the
// playlist's query (inheritQuery).
func resolve(base *url.URL, ref string) (string, error) {
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", badPlaylist("bad URI")
	}
	u := base.ResolveReference(r)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", badPlaylist("URI scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", badPlaylist("URI without a host")
	}
	u.Fragment = ""
	inheritQuery(base, u)
	s := u.String()
	if _, err := url.Parse(s); err != nil { // ("//::" parses, "https://::" does not; fuzz)
		return "", badPlaylist("bad URI")
	}
	return s, nil
}

// inheritQuery gives u the query of the playlist it was read from (lead's
// ruling 6): CDNs signed with a query (CloudFront's Policy, Signature and
// Key-Pair-Id on Echo360's public links) list segments, sub-playlists, keys
// and init sections by bare relative names that must be requested with the
// playlist's own signature; players do the same. Only for URIs without a
// query and only on the playlist's own scheme and host, so a signature
// never travels to another server. The whole query is taken: which of its
// parameters sign the request is the CDN's business, and an unneeded one
// is ignored by the servers seen so far.
func inheritQuery(base, u *url.URL) {
	if u.RawQuery != "" || u.ForceQuery || base.RawQuery == "" {
		return
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) || !sameHost(u.Host, base.Host) {
		return
	}
	u.RawQuery = base.RawQuery
}

// planBudget counts the bytes of the URLs a playlist or an MPD plans
// (maxPlanBytes).
type planBudget struct{ n int }

func (b *planBudget) add(u string) error {
	b.n += len(u)
	if b.n > maxPlanBytes {
		return badPlaylist("the playlist lists too many URLs (%d bytes)", b.n)
	}
	return nil
}

func parseMaster(body []byte, base *url.URL) (*masterPlaylist, error) {
	lines, err := playlistLines(body)
	if err != nil {
		return nil, err
	}
	m := &masterPlaylist{}
	var budget planBudget
	var pending *hlsVariant
	for _, l := range lines {
		switch {
		case l == "":
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF:"):
			a := attrs(l[len("#EXT-X-STREAM-INF:"):])
			v := hlsVariant{codecs: a["CODECS"], audio: a["AUDIO"]}
			v.bandwidth, _ = strconv.ParseInt(a["BANDWIDTH"], 10, 64)
			if w, h, ok := strings.Cut(a["RESOLUTION"], "x"); ok {
				wi, _ := strconv.ParseInt(w, 10, 64)
				hi, _ := strconv.ParseInt(h, 10, 64)
				v.pixels = wi * hi
			}
			pending = &v
		case strings.HasPrefix(l, "#EXT-X-MEDIA:"):
			a := attrs(l[len("#EXT-X-MEDIA:"):])
			r := hlsRendition{typ: strings.ToUpper(a["TYPE"]), group: a["GROUP-ID"], def: strings.EqualFold(a["DEFAULT"], "YES")}
			if a["URI"] != "" {
				if r.uri, err = resolve(base, a["URI"]); err != nil {
					return nil, err
				}
				if err := budget.add(r.uri); err != nil {
					return nil, err
				}
			}
			m.renditions = append(m.renditions, r)
		case strings.HasPrefix(l, "#"):
		default:
			if pending == nil {
				continue
			}
			if pending.uri, err = resolve(base, l); err != nil {
				return nil, err
			}
			if err := budget.add(pending.uri); err != nil {
				return nil, err
			}
			m.variants = append(m.variants, *pending)
			pending = nil
		}
	}
	if len(m.variants) == 0 {
		return nil, badPlaylist("master playlist without variants")
	}
	return m, nil
}

var videoCodecPrefixes = []string{"avc", "hvc", "hev", "av01", "vp09", "vp9", "vp8", "mp4v", "dvh", "dva"}

// audioOnly reports whether a CODECS list names no video codec.
func audioOnly(codecs string) bool {
	if strings.TrimSpace(codecs) == "" {
		return false
	}
	for _, c := range strings.Split(codecs, ",") {
		c = strings.ToLower(strings.TrimSpace(c))
		for _, p := range videoCodecPrefixes {
			if strings.HasPrefix(c, p) {
				return false
			}
		}
	}
	return true
}

func (m *masterPlaylist) audioRendition(group string) string {
	var first string
	for _, r := range m.renditions {
		if r.typ != "AUDIO" || r.uri == "" || (group != "" && r.group != group) {
			continue
		}
		if r.def {
			return r.uri
		}
		if first == "" {
			first = r.uri
		}
	}
	return first
}

// pick chooses the media playlist of a track of kind. For video it is the
// variant of the highest bandwidth (then resolution); when that variant's
// audio is a separate rendition, extraAudio is the rendition to add (used
// only when the task has no audio track of its own). For audio it is the
// default audio rendition, else an audio-only variant, else the smallest
// variant.
func (m *masterPlaylist) pick(kind string) (media, extraAudio string) {
	vs := append([]hlsVariant(nil), m.variants...)
	if kind == "audio" {
		if r := m.audioRendition(""); r != "" {
			return r, ""
		}
		sort.SliceStable(vs, func(i, j int) bool { return vs[i].bandwidth > vs[j].bandwidth })
		for _, v := range vs {
			if audioOnly(v.codecs) {
				return v.uri, ""
			}
		}
		return vs[len(vs)-1].uri, ""
	}
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].bandwidth != vs[j].bandwidth {
			return vs[i].bandwidth > vs[j].bandwidth
		}
		return vs[i].pixels > vs[j].pixels
	})
	top := vs[0]
	for _, v := range vs {
		if !audioOnly(v.codecs) {
			top = v
			break
		}
	}
	if top.audio != "" {
		extraAudio = m.audioRendition(top.audio)
	}
	return top.uri, extraAudio
}

// Media playlists.

const (
	lineText = iota
	lineSegment
	lineKey
	lineMap
)

type mediaLine struct {
	text string
	kind int
	uri  string    // absolute, for lineSegment / lineKey / lineMap
	rng  byteRange // lineSegment, lineMap: the sub-range of uri
	iv   string    // lineKey: the IV attribute ("" = none)
}

// byteRange is the sub-range of a segment or init section (EXT-X-BYTERANGE,
// EXT-X-MAP BYTERANGE); ok false = the whole resource.
type byteRange struct {
	ok          bool
	off, length int64
}

type mediaPlaylist struct {
	lines     []mediaLine
	resources []string          // unique absolute URLs, in playlist order
	names     map[string]string // URL → local file name
	segments  int
	encrypted bool        // an AES-128 key applies to some segment
	ranges    []byteRange // per segment and init section, in playlist order
	rangeURIs []string    // the URI of each entry of ranges
}

// singleFile reports the "单文件快速路径" (lead, M0 on Panopto): every
// segment and init section is a sub-range of one URI, the ranges start at
// 0 and follow each other without a gap, and nothing is encrypted. That
// URI is then downloaded as one file (aria2c splits it), which must be at
// least minSize bytes (the end of the last range), and remuxed locally.
func (m *mediaPlaylist) singleFile() (uri string, minSize int64, ok bool) {
	if m.encrypted || len(m.ranges) == 0 || len(m.resources) != 1 {
		return "", 0, false
	}
	var next int64
	for i, r := range m.ranges {
		if !r.ok || r.length <= 0 || r.off != next || m.rangeURIs[i] != m.resources[0] {
			return "", 0, false
		}
		next = r.off + r.length
	}
	return m.resources[0], next, true
}

// parseRange reads "n[@o]"; without @o the range follows the previous one
// of the same URI (prevEnd). The end of a range must fit an int64.
func parseRange(v string, prevEnd int64) (byteRange, error) {
	n, o, hasOff := strings.Cut(strings.TrimSpace(v), "@")
	length, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
	if err != nil || length < 0 {
		return byteRange{}, badPlaylist("bad byte range")
	}
	off := prevEnd
	if hasOff {
		if off, err = strconv.ParseInt(strings.TrimSpace(o), 10, 64); err != nil || off < 0 {
			return byteRange{}, badPlaylist("bad byte range")
		}
	}
	if off >= 0 && length > math.MaxInt64-off {
		return byteRange{}, badPlaylist("bad byte range")
	}
	return byteRange{ok: true, off: off, length: length}, nil
}

// ivRe is an EXT-X-KEY IV: a hexadecimal number of at most 128 bits.
var ivRe = regexp.MustCompile(`^0[xX][0-9a-fA-F]{1,32}$`)

func parseMedia(body []byte, base *url.URL) (*mediaPlaylist, error) {
	lines, err := playlistLines(body)
	if err != nil {
		return nil, err
	}
	m := &mediaPlaylist{names: map[string]string{}}
	ended := false
	nk, nm := 0, 0
	keyed := false                // the current key is AES-128
	var pending *byteRange        // EXT-X-BYTERANGE of the next segment
	lastEnd := map[string]int64{} // end of the last sub-range per URI
	var budget planBudget
	add := func(u, name string) {
		if _, ok := m.names[u]; !ok {
			m.names[u] = name
			m.resources = append(m.resources, u)
		}
	}
	for _, l := range lines {
		switch {
		case l == "":
			m.lines = append(m.lines, mediaLine{text: l})
		case l == "#EXT-X-ENDLIST":
			ended = true
			m.lines = append(m.lines, mediaLine{text: l})
		case strings.HasPrefix(l, "#EXT-X-PLAYLIST-TYPE:"):
			if strings.EqualFold(strings.TrimSpace(l[len("#EXT-X-PLAYLIST-TYPE:"):]), "VOD") {
				ended = true
			}
			m.lines = append(m.lines, mediaLine{text: l})
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF"):
			return nil, badPlaylist("master playlist where a media playlist was expected")
		case strings.HasPrefix(l, "#EXT-X-KEY:"), strings.HasPrefix(l, "#EXT-X-SESSION-KEY:"):
			tag := l[:strings.IndexByte(l, ':')]
			a := attrs(l[len(tag)+1:])
			method := strings.ToUpper(a["METHOD"])
			if kf := a["KEYFORMAT"]; kf != "" && !strings.EqualFold(kf, "identity") {
				return nil, &errPlaylist{feature: "drm"}
			}
			// The local playlist gets the key line as this parser read it
			// (local), not as the server wrote it: ffmpeg reads attributes
			// otherwise (case, unquoted values, escapes), and a URI it read
			// differently could name any local file.
			switch method {
			case "NONE":
				keyed = false
				m.lines = append(m.lines, mediaLine{text: tag + ":METHOD=NONE"})
			case "AES-128":
				keyed = true
				if a["URI"] == "" {
					return nil, badPlaylist("AES-128 key without URI")
				}
				iv := a["IV"]
				if iv != "" && !ivRe.MatchString(iv) {
					return nil, badPlaylist("bad IV")
				}
				u, err := resolve(base, a["URI"])
				if err != nil {
					return nil, err
				}
				if err := budget.add(u); err != nil {
					return nil, err
				}
				if _, ok := m.names[u]; !ok {
					nk++
					add(u, fmt.Sprintf("k%03d.key", nk))
				}
				m.lines = append(m.lines, mediaLine{text: tag + ":METHOD=AES-128", kind: lineKey, uri: u, iv: iv})
			case "SAMPLE-AES", "SAMPLE-AES-CTR", "SAMPLE-AES-CENC":
				return nil, &errPlaylist{feature: "sample-aes"}
			default:
				return nil, badPlaylist("key method %q", method)
			}
		case strings.HasPrefix(l, "#EXT-X-MAP:"):
			a := attrs(l[len("#EXT-X-MAP:"):])
			u, err := resolve(base, a["URI"])
			if err != nil || a["URI"] == "" {
				return nil, badPlaylist("EXT-X-MAP without URI")
			}
			if err := budget.add(u); err != nil {
				return nil, err
			}
			if _, ok := m.names[u]; !ok {
				nm++
				add(u, fmt.Sprintf("m%03d%s", nm, hlsPartExt(u, ".mp4")))
			}
			r := byteRange{}
			if v := a["BYTERANGE"]; v != "" {
				if r, err = parseRange(v, 0); err != nil {
					return nil, err
				}
				lastEnd[u] = r.off + r.length
			}
			m.ranges, m.rangeURIs = append(m.ranges, r), append(m.rangeURIs, u)
			m.encrypted = m.encrypted || keyed
			m.lines = append(m.lines, mediaLine{text: "#EXT-X-MAP:", kind: lineMap, uri: u, rng: r})
		case strings.HasPrefix(l, "#EXT-X-BYTERANGE:"):
			v := l[len("#EXT-X-BYTERANGE:"):]
			// the offset of a range without @o is resolved with its URI
			// below; the local playlist writes it out with its segment
			r, err := parseRange(v, -1)
			if err != nil {
				return nil, err
			}
			pending = &r
		case strings.HasPrefix(l, "#"):
			m.lines = append(m.lines, mediaLine{text: l})
		default:
			u, err := resolve(base, l)
			if err != nil {
				return nil, err
			}
			if err := budget.add(u); err != nil {
				return nil, err
			}
			if m.segments >= maxHLSSegments {
				return nil, badPlaylist("too many segments")
			}
			if _, ok := m.names[u]; !ok {
				add(u, fmt.Sprintf("s%05d%s", len(m.names)+1-nk-nm, hlsPartExt(u, ".ts")))
			}
			r := byteRange{}
			if pending != nil {
				r = *pending
				if r.off < 0 {
					// follows the previous sub-range of this URI (players
					// also accept it after the init section, or at 0)
					r.off = lastEnd[u]
					if r.length > math.MaxInt64-r.off {
						return nil, badPlaylist("bad byte range")
					}
				}
				lastEnd[u] = r.off + r.length
				pending = nil
			}
			m.ranges, m.rangeURIs = append(m.ranges, r), append(m.rangeURIs, u)
			m.encrypted = m.encrypted || keyed
			m.segments++
			m.lines = append(m.lines, mediaLine{text: l, kind: lineSegment, uri: u, rng: r})
		}
	}
	if m.segments == 0 {
		return nil, badPlaylist("media playlist without segments")
	}
	if !ended {
		return nil, &errPlaylist{feature: "live"}
	}
	return m, nil
}

// localTags are the tags the local playlist keeps besides the keys, init
// sections and byte ranges it writes itself: those the bundled ffmpeg
// reads that name no other resource (an EXT-X-KEY here is a METHOD=NONE
// as parseMedia rewrote it). Comments and any other tag (an EXT-X-MEDIA,
// say, whose URI ffmpeg would open) are left out.
var localTags = map[string]bool{
	"#EXT-X-KEY": true, "#EXT-X-SESSION-KEY": true, "#EXTM3U": true, "#EXT-X-VERSION": true, "#EXT-X-TARGETDURATION": true, "#EXT-X-MEDIA-SEQUENCE": true,
	"#EXT-X-DISCONTINUITY-SEQUENCE": true, "#EXT-X-PLAYLIST-TYPE": true, "#EXT-X-ENDLIST": true, "#EXTINF": true,
	"#EXT-X-DISCONTINUITY": true, "#EXT-X-PROGRAM-DATE-TIME": true, "#EXT-X-INDEPENDENT-SEGMENTS": true,
	"#EXT-X-START": true, "#EXT-X-GAP": true, "#EXT-X-BITRATE": true,
}

// local renders the playlist for the bundled ffmpeg with every URI
// replaced by its local file name (relative to the playlist's
// directory): segments, keys and init sections as this parser read them,
// so that ffmpeg reads no other file.
func (m *mediaPlaylist) local() []byte {
	var b strings.Builder
	for _, l := range m.lines {
		switch l.kind {
		case lineSegment:
			if l.rng.ok {
				fmt.Fprintf(&b, "#EXT-X-BYTERANGE:%d@%d\n", l.rng.length, l.rng.off)
			}
			b.WriteString(m.names[l.uri])
		case lineKey:
			b.WriteString(l.text + `,URI="` + m.names[l.uri] + `"`)
			if l.iv != "" {
				b.WriteString(",IV=" + l.iv)
			}
		case lineMap:
			b.WriteString(l.text + `URI="` + m.names[l.uri] + `"`)
			if l.rng.ok {
				fmt.Fprintf(&b, `,BYTERANGE="%d@%d"`, l.rng.length, l.rng.off)
			}
		default:
			tag, _, _ := strings.Cut(l.text, ":")
			if l.text != "" && !localTags[tag] {
				continue
			}
			b.WriteString(l.text)
		}
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// hlsSegmentExts are segment and init-section extensions of HLS that are
// no lecture media extension (media.Exts): fragmented MP4 / CMAF parts and
// packed audio.
var hlsSegmentExts = map[string]bool{".m4s": true, ".cmfv": true, ".cmfa": true, ".ac3": true, ".ec3": true, ".mp2": true}

// neutralPartExt is the extension of a segment or init section whose URL
// names none of a media file.
const neutralPartExt = ".seg"

// hlsPartExt is the extension a downloaded HLS segment or init section is
// saved with (its local file name, the one the local playlist names): the
// URL's (extOf) when it is that of a media file (media.Exts or
// hlsSegmentExts), def when the URL has none, else the neutral .seg. Never
// .m3u8 or any other playlist's: the local playlist's whitelist allows the
// hls demuxer (media.HLSFormatWhitelist), and ffmpeg opens a segment
// named .m3u8 as a nested playlist (security review 3, I1; FFmpeg itself
// refuses the entries of such a nested playlist today, but that defence is
// not ours). ffmpeg identifies a segment by its content
// (-allowed_segment_extensions ALL -extension_picky 0, merge.go), so the
// name only has to be harmless; joinTrack also refuses a part whose first
// bytes are a playlist.
func hlsPartExt(rawURL, def string) string {
	e := extOf(rawURL, def)
	if e == def || lfmedia.AllowedName("part"+e) || hlsSegmentExts[e] {
		return e
	}
	return neutralPartExt
}

var extRe = regexp.MustCompile(`^\.[a-z0-9]{1,5}$`)

// extOf is the lower-case extension of a URL's path, or def when it has
// none usable.
func extOf(rawURL, def string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return def
	}
	e := strings.ToLower(path.Ext(u.Path))
	if !extRe.MatchString(e) {
		return def
	}
	return e
}
