// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The accounts the diagnostics redact (v3.2.x r4, T19 F2). The data
// directory's permission check names the other accounts that may reach it
// (dirperm: "DOMAIN\name" or a SID on Windows, "group <name>" on Unix), in
// its row of checks.json and in a WARN of the logs. On the diagnostics page
// they stay as they are; in the zip, by the same rules in every file:
//
//   - SIDs: the machine (or domain) part of S-1-5-21-a-b-c-RID becomes
//     S-1-5-21-<machine>-RID; an Entra ID object (S-1-12-1-a-b-c-d, its
//     GUID) becomes S-1-12-1-<account> and a Microsoft account
//     (S-1-11-96-…) S-1-11-96-<account>. Other SIDs (S-1-1-0, S-1-5-11,
//     S-1-5-18, S-1-5-32-545, S-1-5-80-…, …) are the same on every machine
//     and stay.
//   - Qualified names "DOMAIN\name" (also "DOMAIN\\name", JSON and quoted
//     log values): DOMAIN becomes <host> when it is this machine's name
//     (host name, COMPUTERNAME) and <domain> otherwise; name becomes
//     <account-N>, one N per name (ASCII case ignored) in the whole zip.
//     The system's domains (BUILTIN, NT AUTHORITY, NT SERVICE, …, and
//     this machine's localised names of them, by their well-known SIDs:
//     localSystemDomains) stay with their names. Found everywhere for this
//     machine's name and the user's domain (USERDOMAIN); any other domain
//     is found inside a "who"
//     value (who="…", "who": "…") and, once seen, everywhere in the zip.
//     A name runs to ", " in a who value, elsewhere to a space or a
//     character Windows does not allow in account names; outside a who
//     value a name learned from one is matched whole first (it may hold
//     spaces). In a JSON file a backslash of a name is always "\\": a
//     single one starts an escape ("\n", "\u003e") and is no separator;
//     so in a quoted value of the server's log (inSlogQuote, v3.2.x r5).
//     Elsewhere a single backslash is one (an unquoted log value,
//     who=DOMAIN\name, and other logs' lines).
//
// Which files are JSON is decided by their names in the zip, never by
// their content (a log cut to the budget starts with "[earlier lines
// omitted]"; SEC34b), and learn and apply are given the same answer
// (postDiagnostics, diagFileIsJSON):
//   - JSON: info.json, app-settings.json, checks.json, hardware.json,
//     database.json and the sections' files (colab.json,
//     remote-servers.json, any later DiagSection): every "*.json".
//   - text: README.txt and everything under logs/ (lectureflow.log, its
//     rotated files, desktop.log, …), whatever their first line.
// redactor.apply (outside the zip's files) treats its text as not JSON.
//   - Names without a domain stay: dirperm drops the domain of the
//     system's principals only (Users, Authenticated Users, Everyone,
//     SYSTEM). "group <name>" in a who value becomes "group <account-N>"
//     unless the group is the system's (root, wheel, staff, users, …);
//     once seen, "group <name>" becomes so everywhere in the zip (the
//     checks' detail text, rendered from a template, holds the same
//     "{who}" as the who value).
//   - This machine's name as a whole word elsewhere becomes <host> (a
//     generic one such as "desktop", or a word of the diagnostics such as
//     "data", only in an address, as remote servers' host names;
//     remote_redact.go, diagVocabulary). Its full DNS name is <host>
//     whole; its DNS suffix ends other host names as <domain>.
//   - The server of a UNC path ("\\server\share"), whichever it is,
//     becomes <host> (redactUNCHosts).
//
// The diagnostics learn the who values of every file before they redact
// any (postDiagnostics): a name whose who value is only in an older log
// is replaced in a newer one too.

import (
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	redactedDomain  = "<domain>"
	redactedMachine = "<machine>"
)

// localAccountDomains are this machine's names (hosts; a full DNS name
// too: the host name when it has dots, COMPUTERNAME.USERDNSDOMAIN on a
// domain-joined Windows) and the user's domain when it is not one of them
// (domains).
var localAccountDomains = func() (hosts, domains []string) {
	if h, err := os.Hostname(); err == nil {
		hosts = append(hosts, h)
		if i := strings.IndexByte(h, '.'); i > 0 {
			hosts = append(hosts, h[:i])
		}
	}
	hosts = append(hosts, os.Getenv("COMPUTERNAME"))
	if c, d := os.Getenv("COMPUTERNAME"), strings.Trim(os.Getenv("USERDNSDOMAIN"), "."); c != "" && d != "" {
		hosts = append(hosts, c+"."+d)
	}
	if d := os.Getenv("USERDOMAIN"); !slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, d) }) {
		domains = append(domains, d)
	}
	return hosts, domains
}

// systemDomains are the domains of the system's own principals (upper case),
// by their English names; this machine's own names of them (a localised
// Windows: "VORDEFINIERT", "NT-AUTORITÄT") come from localSystemDomains.
var systemDomains = map[string]bool{
	"BUILTIN": true, "NT AUTHORITY": true, "NT SERVICE": true, "NT VIRTUAL MACHINE": true, "NT TASK": true,
	"APPLICATION PACKAGE AUTHORITY": true, "IIS APPPOOL": true, "WINDOW MANAGER": true, "FONT DRIVER HOST": true,
}

// localSystemDomains are this machine's names of the system's domains,
// looked up by the well-known SIDs of their principals (S-1-5-32-544
// BUILTIN, S-1-5-18 NT AUTHORITY, …) whatever the system's language
// (SEC34 信息-5): Windows names them in its own language, and dirperm
// writes "DOMAIN\name" with those names. Not Windows: none
// (redact_account_windows.go, redact_account_other.go).
var localSystemDomains = systemDomainsOfSIDs

// diagVocabulary are words of the diagnostics themselves (keys and values
// of the zip's JSON files, words of the logs): a host name that is one of
// them names no one and is replaced in a host's places only (formHost, a
// UNC path, "HOST\name"), as a generic remote host is (SEC34 信息-4: a
// computer named "data" hid every "data" of the zip). Lower case.
var diagVocabulary = map[string]bool{
	"data": true, "app": true, "api": true, "web": true, "www": true, "file": true, "files": true, "media": true,
	"audio": true, "video": true, "videos": true, "music": true, "photos": true, "pictures": true, "documents": true,
	"download": true, "downloads": true, "upload": true, "uploads": true, "model": true, "models": true, "cache": true,
	"config": true, "settings": true, "log": true, "logs": true, "temp": true, "tmp": true, "backup": true,
	"backups": true, "storage": true, "share": true, "shared": true, "public": true, "export": true, "import": true,
	"update": true, "updates": true, "lecture": true, "lectures": true, "course": true, "courses": true, "job": true,
	"jobs": true, "stage": true, "task": true, "tasks": true, "info": true, "debug": true, "error": true,
	"status": true, "system": true, "network": true, "disk": true, "memory": true, "python": true, "client": true,
	"proxy": true, "mirror": true, "database": true, "library": true, "school": true, "student": true,
	"teacher": true, "classroom": true,
}

// systemGroups are Unix groups every machine has (lower case).
var systemGroups = map[string]bool{
	"root": true, "wheel": true, "staff": true, "admin": true, "adm": true, "sudo": true, "users": true,
	"everyone": true, "nogroup": true, "nobody": true, "daemon": true, "sys": true, "bin": true, "operator": true,
}

var (
	machineSIDRe = regexp.MustCompile(`\bS-1-5-21-\d+-\d+-\d+\b`)
	entraSIDRe   = regexp.MustCompile(`\bS-1-12-1-\d+-\d+-\d+-\d+\b`)
	msaSIDRe     = regexp.MustCompile(`\bS-1-11-96(?:-\d+)+\b`)
	whoKeyRe     = regexp.MustCompile(`(?i)\bwho"?\s*[:=]\s*`)
)

// accountRedactor holds one zip's placeholders.
type accountRedactor struct {
	mu      sync.Mutex
	hosts   []string            // this machine's names, lower case
	domains map[string]string   // lower-case domain → <host> or <domain>
	known   map[string][]string // lower-case domain → its learned names (lower case), longest first
	labels  map[string]string   // lower-case account name → <account-N>
	// "lower case" is ASCII only (asciiLower): the byte offsets stay
	groups []string // Unix groups learned from who values, longest first
	// the DNS suffixes of this machine's full names (lower case, longest
	// first): an organisation's domain (dnsSuffixOf)
	suffixes []string
	sys      map[string]bool // the system's domains (upper case): systemDomains, localSystemDomains
}

// genericDNSSuffixes name no one: a home network's, a router's or Windows'
// default (one-label suffixes such as "local" or "lan" never count).
var genericDNSSuffixes = map[string]bool{
	"home.arpa": true, "in-addr.arpa": true, "ip6.arpa": true, "fritz.box": true, "mshome.net": true,
}

// dnsSuffixOf is the DNS suffix of a full host name ("ws.cs.uni.example" →
// "cs.uni.example"): the part after its first label when that has two
// labels or more (one label could be a file's extension) and is not a
// generic suffix; "" otherwise.
func dnsSuffixOf(host string) string {
	_, suf, ok := strings.Cut(host, ".")
	if !ok || !strings.Contains(suf, ".") || genericDNSSuffixes[suf] || strings.HasPrefix(suf, ".") || strings.HasSuffix(suf, ".") {
		return ""
	}
	return suf
}

// diagFileIsJSON: the zip's file name is a JSON file's (see above).
func diagFileIsJSON(name string) bool {
	return !strings.HasPrefix(name, "logs/") && strings.HasSuffix(strings.ToLower(name), ".json")
}

func newAccountRedactor(hosts, domains []string) *accountRedactor {
	a := &accountRedactor{domains: map[string]string{}, known: map[string][]string{}, labels: map[string]string{},
		sys: map[string]bool{}}
	for d := range systemDomains {
		a.sys[d] = true
	}
	for _, d := range localSystemDomains() {
		if d = strings.TrimSpace(d); d != "" {
			a.sys[strings.ToUpper(d)] = true
		}
	}
	for _, h := range hosts {
		if h = asciiLower(strings.TrimSpace(h)); len(h) >= 2 && !slices.Contains(a.hosts, h) {
			a.hosts = append(a.hosts, h)
			a.domains[h] = redactedHost
			if suf := dnsSuffixOf(h); suf != "" && !slices.Contains(a.suffixes, suf) {
				a.suffixes = append(a.suffixes, suf)
			}
		}
	}
	// a full name before its first label ("ws.corpx.example" whole)
	slices.SortStableFunc(a.hosts, func(x, y string) int { return len(y) - len(x) })
	slices.SortStableFunc(a.suffixes, func(x, y string) int { return len(y) - len(x) })
	for _, d := range domains {
		a.addDomain(d)
	}
	return a
}

func (a *accountRedactor) addDomain(d string) {
	d = asciiLower(strings.TrimSpace(d))
	if len(d) < 2 || a.sys[strings.ToUpper(d)] {
		return
	}
	if _, ok := a.domains[d]; !ok {
		a.domains[d] = redactedDomain
	}
}

func (a *accountRedactor) label(name string) string {
	k := strings.ToLower(name)
	if l, ok := a.labels[k]; ok {
		return l
	}
	l := "<account-" + strconv.Itoa(len(a.labels)+1) + ">"
	a.labels[k] = l
	return l
}

// learn notes the domains and names of s's who values (the diagnostics
// learn every file before redacting any: one placeholder per name).
func (a *accountRedactor) learn(s string, js bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.whoValues(s, js)
}

func (a *accountRedactor) apply(s string, js bool) string {
	if a == nil {
		return s
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s = machineSIDRe.ReplaceAllLiteralString(s, "S-1-5-21-"+redactedMachine)
	s = entraSIDRe.ReplaceAllLiteralString(s, "S-1-12-1-<account>")
	s = msaSIDRe.ReplaceAllLiteralString(s, "S-1-11-96-<account>")
	s = a.whoValues(s, js)
	s = a.qualified(s, js)
	for _, g := range a.groups {
		s = replaceWord(s, "group "+g, "group "+a.label(g), false, 0)
	}
	s = redactUNCHosts(s, js)
	for _, h := range a.hosts {
		var form wordForm
		if genericRemoteHosts[h] || genericRemoteWords[h] || diagVocabulary[h] || h == "localhost" {
			form = formHost
		}
		s = replaceWord(s, h, redactedHost, true, form)
	}
	for _, suf := range a.suffixes {
		s = replaceDNSSuffix(s, suf)
	}
	return s
}

// redactUNCHosts replaces the server of a UNC path, "\\server\share", with
// <host>, whatever server it is (SEC34 L-3: a data directory on a network
// share names the file server and the organisation's domain). A UNC path
// starts a token (at the start, after white space, a quote, "=", "(", "[",
// "," or ";"; never after "C:" or inside a path) with two backslashes, four
// when escaped (JSON, a quoted log value; a JSON file has only those), eight
// when escaped twice (Python's repr of the path in a quoted log value or a
// JSON file; SEC35 L-2), and its server, a host name (letters of any
// script), is followed by the same separator (half the backslashes). "\\?\UNC\server\"
// is one too; the other device paths ("\\?\C:\", "\\.\pipe\"), localhost
// and an address (the address rules' <ip>) stay.
func redactUNCHosts(s string, js bool) string {
	if !strings.Contains(s, `\\`) {
		return s
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		k := strings.Index(s[i:], `\\`)
		if k < 0 {
			break
		}
		k += i
		n := 0 // the backslashes in a row
		for k+n < len(s) && s[k+n] == '\\' {
			n++
		}
		i = k + n
		if n != 4 && n != 8 && (js || n != 2) || k > 0 && strings.IndexByte(" \t\r\n\"'=([,;", s[k-1]) < 0 {
			continue
		}
		sep := s[k : k+n/2] // "\", "\\" or "\\\\"
		at := k + n
		if rest, ok := strings.CutPrefix(s[at:], "?"+sep+"UNC"+sep); ok {
			at = len(s) - len(rest)
		}
		end := at
		for end < len(s) && (isWordByte(s[end]) || s[end] == '-' || s[end] == '.' || s[end] >= 0x80) {
			end++
		}
		server := s[at:end]
		if server == "" || strings.Trim(server, ".") == "" || !strings.HasPrefix(s[end:], sep) ||
			strings.EqualFold(server, "localhost") || isAddr(server) {
			continue
		}
		b.WriteString(s[last:at])
		b.WriteString(redactedHost)
		last, i = end, end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// replaceDNSSuffix replaces this machine's DNS suffix with <domain> where
// it ends a host name ("fs03.corpx.example" → "fs03.<domain>") or is one
// ("CORPX.EXAMPLE", "a@corpx.example"): before it a dot or no part of a
// name, after it no part of a name ("corpx.example.org" is another).
func replaceDNSSuffix(s, suf string) string {
	low := asciiLower(s)
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		k := strings.Index(low[i:], suf)
		if k < 0 {
			break
		}
		k += i
		end := k + len(suf)
		i = k + 1
		if k > 0 && (isWordByte(s[k-1]) || s[k-1] == '-') {
			continue
		}
		if end < len(s) && (isWordByte(s[end]) || s[end] == '-' || s[end] == '.' && end+1 < len(s) && isWordByte(s[end+1])) {
			continue
		}
		b.WriteString(s[last:k])
		b.WriteString(redactedDomain)
		last, i = end, end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// whoValues rewrites the items of each who value (and learns them).
func (a *accountRedactor) whoValues(s string, js bool) string {
	idx := whoKeyRe.FindAllStringIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		start := m[1]
		if start < last {
			continue
		}
		end := start
		if start < len(s) && s[start] == '"' { // quoted: up to the closing quote
			start++
			for end = start; end < len(s) && s[end] != '"' && s[end] != '\n'; end++ {
				if s[end] == '\\' {
					end++
				}
			}
			end = min(end, len(s))
		} else {
			for end < len(s) && s[end] != ' ' && s[end] != '\t' && s[end] != '\r' && s[end] != '\n' {
				end++
			}
		}
		b.WriteString(s[last:start])
		// a quoted value of the server's log is escaped as JSON text is
		b.WriteString(a.whoValue(s[start:end], js || start > m[1] && inSlogQuote(s, start)))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// whoValue rewrites one who value: items joined by ", ", maybe in "[…]".
func (a *accountRedactor) whoValue(v string, js bool) string {
	pre, post := "", ""
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		pre, post, v = "[", "]", v[1:len(v)-1]
	}
	items := strings.Split(v, ", ")
	for i, it := range items {
		items[i] = a.whoItem(it, js)
	}
	return pre + strings.Join(items, ", ") + post
}

func (a *accountRedactor) whoItem(it string, js bool) string {
	if g, ok := strings.CutPrefix(it, "group "); ok {
		if g = strings.TrimSpace(g); g == "" || systemGroups[strings.ToLower(g)] || isAllDigits(g) {
			return it
		}
		if !slices.Contains(a.groups, g) {
			a.groups = append(a.groups, g)
			slices.SortFunc(a.groups, func(x, y string) int { return len(y) - len(x) })
		}
		return "group " + a.label(g)
	}
	i := strings.IndexByte(it, '\\')
	if i <= 0 {
		return it
	}
	dom := strings.TrimSpace(it[:i])
	if dom == "" || strings.ContainsAny(dom, `:/<>`) || a.sys[strings.ToUpper(dom)] {
		return it // a path, "not confirmed: …", a placeholder or the system's
	}
	sep := `\`
	if strings.HasPrefix(it[i:], `\\`) {
		sep = `\\`
	} else if js {
		return it // an escape, not a separator
	}
	name := strings.TrimSpace(it[i+len(sep):])
	if name == "" {
		return it
	}
	a.addDomain(dom)
	k := asciiLower(dom)
	if lname := asciiLower(name); !slices.Contains(a.known[k], lname) {
		a.known[k] = append(a.known[k], lname)
		slices.SortFunc(a.known[k], func(x, y string) int { return len(y) - len(x) })
	}
	return a.domains[k] + sep + a.label(name)
}

func isAddr(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigitByte(s[i]) {
			return false
		}
	}
	return s != ""
}

// accountNameStop ends a name outside a who value: white space and the
// characters Windows does not allow in account names (and brackets).
const accountNameStop = " \t\r\n\\/\"[]:;|=,+*?<>()（）、，；："

// qualified rewrites "DOMAIN\name" anywhere for the known domains. A domain
// right after a letter, digit, "_", "-", ".", "\" or "/" is part of a longer
// word or a path ("\\HOST\share" keeps its share; the bare name rule
// replaces HOST).
func (a *accountRedactor) qualified(s string, js bool) string {
	if len(a.domains) == 0 || !strings.Contains(s, `\`) {
		return s
	}
	low := asciiLower(s)
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		j := strings.IndexByte(low[i:], '\\')
		if j < 0 {
			break
		}
		j += i
		dom, start := a.domainBefore(low, j)
		if dom == "" || start < last {
			i = j + 1
			continue
		}
		sep := `\`
		if strings.HasPrefix(s[j:], `\\`) {
			sep = `\\`
		} else if js || inSlogQuote(s, j) {
			// (or in a quoted value of the server's log: inSlogQuote)
			i = j + 1 // an escape in JSON text ("\n", "\u003e"), not a separator
			continue
		}
		nameAt := j + len(sep)
		n := 0
		for _, kn := range a.known[dom] {
			if !strings.HasPrefix(low[nameAt:], kn) {
				continue
			}
			if r, _ := utf8.DecodeRuneInString(s[nameAt+len(kn):]); nameAt+len(kn) == len(s) || strings.ContainsRune(accountNameStop, r) {
				n = len(kn)
				break
			}
		}
		if n == 0 {
			if k := strings.IndexFunc(s[nameAt:], func(r rune) bool { return strings.ContainsRune(accountNameStop, r) }); k >= 0 {
				n = k
			} else {
				n = len(s) - nameAt
			}
		}
		if n == 0 {
			i = j + 1
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(a.domains[dom] + sep + a.label(s[nameAt:nameAt+n]))
		last = nameAt + n
		i = last
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// domainBefore: the known domain (lower case) that ends at j in low, the
// longest when several do, and where it starts; "" when none does.
func (a *accountRedactor) domainBefore(low string, j int) (string, int) {
	best, at := "", 0
	for d := range a.domains {
		k := j - len(d)
		if k < 0 || low[k:j] != d || len(d) <= len(best) {
			continue
		}
		if k > 0 {
			if c := low[k-1]; isWordByte(c) || c == '-' || c == '.' || c == '\\' || c == '/' {
				continue
			}
		}
		best, at = d, k
	}
	return best, at
}

// inSlogQuote: s[j] is inside a quoted value of a line of the server's log
// (slog's text handler: the line starts with "time="). slog quotes a value
// with strconv.Quote, so there a backslash is always "\\" and a single one
// starts an escape ("\n", "\t", "\u003e", "\""); an unquoted value keeps
// its backslashes as they are (who=DOMAIN\name). Other logs' lines
// (desktop.log) are not slog's: false (SEC34 信息-3).
func inSlogQuote(s string, j int) bool {
	ls := strings.LastIndexByte(s[:j], '\n') + 1
	if !strings.HasPrefix(s[ls:], "time=") {
		return false
	}
	in := false
	for k := ls; k < j; k++ {
		switch {
		case !in && s[k] == '"':
			in = true
		case in && s[k] == '\\':
			k++ // the escaped character
			if k == j {
				return true // s[j] is the second character of an escape
			}
		case in && s[k] == '"':
			in = false
		}
	}
	return in
}
