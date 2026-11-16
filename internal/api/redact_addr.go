// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The network addresses the diagnostics redact (SEC7 M2): IPv4 and IPv6
// addresses become <ip> (their port stays), the name of a DNS lookup
// ("lookup <name> on …", "lookup <name>: no such host") becomes <host>.
// This machine's loopback (127.0.0.0/8, ::1, localhost) and the
// unspecified addresses (0.0.0.0, ::) stay: they say where the app
// listens and name no one.
//
// The rules keep what only looks like an address:
//   - IPv4: four decimal parts, each 0–255 without a leading zero, not
//     inside a longer word or dotted run (v3.2.0.1, 3.2.0.1.5, 1234.5.6.7,
//     cublas-12.1.3.1, x==12.1.3.1 stay). A first part below 10 is
//     redacted only beside a port or a connection's text ("…:22",
//     "->", "@", "//", "[", "tcp ", "udp ", "addr=", "host=", "ip="): a
//     bare 3.2.0.1 is a version far more often than an address.
//   - IPv6: a run of hex digits, ":" and "." with at least two ":", not
//     glued to a word, that netip parses as an IPv6 address (times such as
//     12:30:45.123, MAC addresses and colon-separated fingerprints do not
//     parse). After "word:" ("key:2001:db8::5") the address is what
//     follows that ":" (N31-L1).

import (
	"net/netip"
	"regexp"
	"strings"
)

// redactFuncs run after redactPatterns. The user information goes first:
// it reads the host it precedes before the address rules replace it.
var redactFuncs = []func(string) string{redactBareUserinfo, redactIPv6, redactIPv4, redactLookupHosts}

// bareUserinfo is "user:password@host" without a scheme (v3.2.x r5, SEC32
// L-4): curl -x, pip --proxy, git and the *_proxy variables take a proxy's
// account so, and a remote installer's stderr may echo one. Only with a
// password (a ":" before the "@"; "user@host" is an address, replaced by
// the remote servers' words), at the start of a token (after white space,
// a quote, "=", "(", "[", "<", ",", ";" or "//"), and only before what is
// surely a host: a name with a dot, a name and a port, or "[" (IPv6).
// Neither part holds "/" or "\" (a path: "C:\x\a:b@c.example",
// "/srv/a:b@c.example"). The password runs to the last such "@" of the
// token (it may hold "@" itself). The user may have a Windows domain,
// "DOMAIN\user", "DOMAIN\\user" escaped (JSON, a quoted log value): the
// domain goes with the user (SEC35 L-1: the account rules replaced only
// the domain and the user, and the password stayed after them).
var bareUserinfo = regexp.MustCompile(`(?m)(^|[\s"'=(\[<,;]|//)((?:[A-Za-z0-9._-]+\\{1,2})?[A-Za-z0-9._%+~-]+):([^\s"'<>/\\]+)@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+|[A-Za-z0-9-]+:[0-9]|\[)`)

// uriSchemesWithoutSlashes are schemes written "scheme:user@host" (no
// password there): "mailto:judy@mail.example" stays.
var uriSchemesWithoutSlashes = map[string]bool{"mailto": true, "sip": true, "sips": true, "xmpp": true, "tel": true,
	"im": true, "pres": true, "news": true}

// digestHost is an image digest after "@" ("alpine:3.19@sha256:…"), no host.
var digestHost = regexp.MustCompile(`(?i)^sha[0-9]+:`)

func redactBareUserinfo(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	idx := bareUserinfo.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		user, host := s[m[4]:m[5]], s[m[8]:]
		if uriSchemesWithoutSlashes[strings.ToLower(user)] || digestHost.MatchString(host) {
			continue
		}
		b.WriteString(s[last:m[4]])
		b.WriteString("<redacted>")
		last = m[7] // the "@" and the host stay
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

const redactedIP = "<ip>"

// asciiLower lowers ASCII letters only (the byte offsets stay).
func asciiLower(s string) string {
	b := []byte(s)
	changed := false
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
			changed = true
		}
	}
	if !changed {
		return s
	}
	return string(b)
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// keptAddr: loopback and unspecified addresses stay.
func keptAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsUnspecified()
}

var ipv4Candidate = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)

// ipv4Contexts are the texts right before an address that make a small
// first part an address (see the rules above).
var ipv4Contexts = []string{"->", "@", "//", "[", "tcp ", "udp ", "tcp4 ", "tcp6 ", "udp4 ", "udp6 ", "addr=", "host=", "ip=", "address="}

func redactIPv4(s string) string {
	idx := ipv4Candidate.FindAllStringIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		if !ipv4Redacted(s, m[0], m[1]) {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(redactedIP)
		last = m[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func ipv4Redacted(s string, i, j int) bool {
	if i > 0 {
		switch c := s[i-1]; {
		case isWordByte(c), c == '.', c == '-', c == '+':
			return false
		}
		if i >= 2 && s[i-2:i] == "==" {
			return false
		}
	}
	if j < len(s) {
		if c := s[j]; isWordByte(c) || c == '.' && j+1 < len(s) && isDigitByte(s[j+1]) {
			return false
		}
	}
	a, err := netip.ParseAddr(s[i:j])
	if err != nil || !a.Is4() || keptAddr(a) {
		return false
	}
	if a.As4()[0] >= 10 {
		return true
	}
	if j+1 < len(s) && s[j] == ':' && isDigitByte(s[j+1]) {
		return true
	}
	before := asciiLower(s[max(0, i-8):i])
	for _, c := range ipv4Contexts {
		if strings.HasSuffix(before, c) {
			return true
		}
	}
	return false
}

func isV6Byte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == ':' || c == '.'
}

func redactIPv6(s string) string {
	if strings.Count(s, ":") < 2 {
		return s
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if !isV6Byte(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isV6Byte(s[j]) {
			j++
		}
		start, end, ok := i, 0, false
		if end, ok = ipv6At(s, i, j); !ok {
			// "key:2001:db8::5", "addr:fe80::1", "Kind:fe80::1": a word,
			// then ":" and the address (N31-L1). The run is glued to the
			// word: it starts at that ":" or inside the word (its last
			// letters are hex digits); the address starts after that
			// first ":". A run not glued to a word keeps its first part.
			if k := strings.IndexByte(s[i:j], ':'); k >= 0 && i > 0 && isWordByte(s[i-1]) {
				start = i + k + 1
				end, ok = ipv6At(s, start, j)
			}
		}
		if ok {
			b.WriteString(s[last:start])
			b.WriteString(redactedIP)
			last = end
		}
		i = j
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// ipv6At: s[i:j] is a run of hex digits, ":" and "."; ok when it (less a
// trailing "." or ":") is an IPv6 address to redact, end where it ends.
func ipv6At(s string, i, j int) (end int, ok bool) {
	if strings.Count(s[i:j], ":") < 2 || i > 0 && isWordByte(s[i-1]) {
		return 0, false
	}
	if j < len(s) && isWordByte(s[j]) { // "…:beef:gx": part of a word
		return 0, false
	}
	for end = j; end > i; end-- {
		a, err := netip.ParseAddr(s[i:end])
		if err == nil {
			return end, a.Is6() && !keptAddr(a)
		}
		if c := s[end-1]; c != '.' && c != ':' {
			return 0, false
		}
	}
	return 0, false
}

// lookupHost is Go's DNS error ("lookup NAME on SERVER: …", "lookup NAME:
// …"); the name is redacted when a resolver's address follows, or ": "
// follows and the name has a dot or the message is a resolver's.
var lookupHost = regexp.MustCompile(`\blookup ([A-Za-z0-9_.\-]+)( on |: )`)

var lookupMessages = []string{"no such host", "server misbehaving", "i/o timeout", "getaddrinfow", "dnsquery",
	"temporary failure", "no answer", "no address"}

func redactLookupHosts(s string) string {
	idx := lookupHost.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		ns, ne, sep := m[2], m[3], s[m[4]:m[5]]
		name := s[ns:ne]
		if strings.EqualFold(name, "localhost") {
			continue
		}
		if a, err := netip.ParseAddr(name); err == nil && keptAddr(a) {
			continue
		}
		// " on " then the resolver: an address ("<ip>" when already redacted)
		resolver := sep == " on " && m[5] < len(s) && strings.IndexByte("0123456789[<", s[m[5]]) >= 0
		redact := resolver || sep == ": " && strings.Contains(name, ".")
		if !redact {
			rest := asciiLower(s[m[5]:min(len(s), m[5]+40)])
			for _, msg := range lookupMessages {
				if strings.HasPrefix(rest, msg) {
					redact = true
					break
				}
			}
		}
		if !redact {
			continue
		}
		b.WriteString(s[last:ns])
		b.WriteString(redactedHost)
		last = ne
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}
