// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Source groups selectable by the user (V3.3: Python packages and models
// independently).
const (
	GroupPyPI   = "pypi"
	GroupModels = "models"
)

// SourceDefault selects a group's default source.
const SourceDefault = "default"

// Sources is the user's download source choice per group: "" or "default",
// the id of one of the manifest's mirrors (e.g. "tuna", "hf-mirror"), or a
// custom http(s) URL that replaces the group's primary placeholder ({pypi}:
// a package index, {models}: a Hugging Face endpoint).
type Sources struct {
	PyPI   string `json:"pypi,omitempty"`
	Models string `json:"models,omitempty"`
}

func (s Sources) choice(group string) string {
	switch group {
	case GroupPyPI:
		return s.PyPI
	case GroupModels:
		return s.Models
	}
	return ""
}

// CodeSourcesInvalid is the HTTP layer's code for an unusable download
// source choice (internal/api; params group, field). The manager returns it
// for a custom package index on plain http to another computer, so that an
// install with such a stored setting fails with the same error as saving
// it. It is listed in the HTTP layer's catalog, not in ErrorCodes.
const CodeSourcesInvalid = "components.sources_invalid"

// InsecureCustomSource reports a custom address for group that must not be
// used: a package index ({pypi}) on plain http to another computer. uv runs
// with --no-build and checks every wheel against the lock file's hashes;
// https also keeps anyone on the way from rewriting the index's answers.
// http stays allowed for this computer (localhost, loopback addresses: a
// local mirror, the tests). Mirror ids and the models group are never
// insecure here (model files are checked against the manifest's SHA-256).
func InsecureCustomSource(group, choice string) bool {
	if group != GroupPyPI {
		return false
	}
	if !looksLikeURL(choice) {
		return false // a mirror id, "", "default"
	}
	u, err := url.Parse(strings.TrimSpace(choice))
	if err != nil {
		// uv's URL parser may read what Go refuses differently
		// ("http://evil\@127.0.0.1/" goes to evil): not judged safe
		return true
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

// looksLikeURL: a source choice that is meant as an address (it starts
// with "http", in any case), not a mirror id.
func looksLikeURL(c string) bool {
	c = strings.TrimSpace(c)
	return len(c) >= 4 && strings.EqualFold(c[:4], "http")
}

// isCustomURL: an http(s) address in any case of the scheme.
func isCustomURL(c string) bool {
	l := strings.ToLower(c)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// MaxCustomURLLen is the longest custom source address.
const MaxCustomURLLen = 2048

// CustomURLOK checks the shape of a custom source address, for the HTTP
// layer (saving the setting) and the manager (using it) alike: at most
// MaxCustomURLLen bytes, an http or https URL Go parses, with a host, and
// without white space, control characters or backslashes (which other
// URL parsers, uv's among them, may read differently).
func CustomURLOK(s string) bool {
	if s == "" || len(s) > MaxCustomURLLen || strings.ContainsRune(s, '\\') {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return false
	}
	return true
}

func insecureSourceError(group string) error {
	return cerr(CodeSourcesInvalid, map[string]any{"group": group, "field": "custom_url"},
		fmt.Errorf("the custom %s index must use https (http only for this computer)", group))
}

// checkCustomChoice refuses a custom address that is malformed
// (CustomURLOK) or, for packages, insecure (InsecureCustomSource).
func checkCustomChoice(g, c string) error {
	if !looksLikeURL(c) {
		return nil
	}
	if !CustomURLOK(c) {
		return cerr(CodeSourcesInvalid, map[string]any{"group": g, "field": "custom_url"},
			fmt.Errorf("the custom %s source is not a usable http(s) address", g))
	}
	if InsecureCustomSource(g, c) {
		return insecureSourceError(g)
	}
	return nil
}

// ValidateSources checks the choices against the manifest's sources.
func (m *Manifest) ValidateSources(s Sources) error {
	for _, g := range []string{GroupPyPI, GroupModels} {
		c := s.choice(g)
		if err := checkCustomChoice(g, c); err != nil {
			return err
		}
		if c == "" || c == SourceDefault || isCustomURL(c) {
			continue
		}
		sg, ok := m.Sources[g]
		found := false
		if ok {
			for _, mr := range sg.Mirrors {
				found = found || mr.ID == c
			}
		}
		if !found {
			return cerr(CodeManifestInvalid, map[string]any{"group": g, "source": c},
				fmt.Errorf("unknown %s source %q", g, c))
		}
	}
	return nil
}

// sourceURL is one URL to try for a download.
type sourceURL struct {
	URL    string
	Group  string // "" when the template has no placeholder
	Mirror string // the mirror id or custom URL; "" = the default source
}

// resolveURL expands a template for the chosen sources: the chosen mirror
// first (when one is chosen), then the default source.
func (m *Manifest) resolveURL(tmpl string, s Sources) ([]sourceURL, error) {
	ph := placeholderOf(tmpl)
	if ph == "" {
		return []sourceURL{{URL: tmpl}}, nil
	}
	name, g := m.groupOf(ph)
	if g == nil {
		return nil, fmt.Errorf("unknown placeholder {%s}", ph)
	}
	expand := func(base string) string {
		return strings.TrimRight(base, "/") + tmpl[len(ph)+2:]
	}
	var out []sourceURL
	if c := s.choice(name); c != "" && c != SourceDefault {
		base := ""
		if isCustomURL(c) {
			if ph == g.Primary {
				base = lowerScheme(c) // for uv and the downloader; the host is left as given for uv and the downloader; the host is left as given
			}
		} else {
			for _, mr := range g.Mirrors {
				if mr.ID == c {
					base = mr.Values[ph]
				}
			}
		}
		if base != "" {
			out = append(out, sourceURL{URL: expand(base), Group: name, Mirror: c})
		}
	}
	def := g.Default.Values[ph]
	if def == "" {
		if len(out) == 0 {
			return nil, fmt.Errorf("no source for {%s}", ph)
		}
		return out, nil
	}
	if len(out) == 0 || out[0].URL != expand(def) {
		out = append(out, sourceURL{URL: expand(def), Group: name})
	}
	return out, nil
}

// indexURLs are the package indexes for uv: the chosen one first, then the
// default.
func (m *Manifest) indexURLs(s Sources) ([]sourceURL, error) {
	g, ok := m.Sources[GroupPyPI]
	if !ok {
		return nil, fmt.Errorf("the manifest has no %q source group", GroupPyPI)
	}
	// checked again where it is used: a task may carry sources validated
	// before this rule existed
	if err := checkCustomChoice(GroupPyPI, s.PyPI); err != nil {
		return nil, err
	}
	return m.resolveURL("{"+g.Primary+"}", s)
}

// redactURL removes the user name and password of a URL (keeping scheme,
// host, path and query) for logs, errors and notices; uv and the
// downloader still get the URL as it is.
func redactURL(s string) string {
	if strings.Contains(s, "@") {
		if u, err := url.Parse(s); err == nil && u.User != nil {
			u.User = nil
			s = u.String()
		}
	}
	return redactText(s)
}

// userinfoRe finds the user information of URLs in a text, up to the last
// "@" before the path ("a@b:pass@host" goes as a whole).
var userinfoRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/?#\s"'<>]*@`)

// secretQueryRe finds query parameters whose name says they carry a
// secret (token, key, password, secret, signature, ...); their values are
// masked, the rest of the query stays.
var secretQueryRe = regexp.MustCompile(`(?i)([?&][^=&#\s"'<>]*(?:token|key|passw|pwd|secret|signature|sig|auth|credential)[^=&#\s"'<>]*=)[^&#\s"'<>]*`)

// redactText removes the user information of every URL in s and masks the
// values of secret-looking query parameters.
func redactText(s string) string {
	if strings.Contains(s, "@") {
		s = userinfoRe.ReplaceAllString(s, "${1}")
	}
	if strings.ContainsAny(s, "?&") {
		s = secretQueryRe.ReplaceAllString(s, "${1}***")
	}
	return s
}

// lowerScheme writes a URL's scheme in lower case (uv 0.13.1 reads
// "HTTPS://host/simple" as a relative path); the rest stays as it is.
func lowerScheme(s string) string {
	if i := strings.Index(s, "://"); i > 0 {
		return strings.ToLower(s[:i]) + s[i:]
	}
	return s
}
