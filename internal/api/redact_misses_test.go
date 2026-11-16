// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// v3.2.x r5 (SEC32 L-4, L-6; SEC34 L-3): forms of credentials, remote
// users and host names the diagnostics used to leave. Every rule has cases
// it replaces and cases it must leave alone. All names, hosts and
// passwords here are made up.

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A URL's user information without a scheme ("user:pass@host" of a proxy
// setting, pip --proxy, curl -x), protocol-relative, with JSON-escaped
// slashes or right after "_".
func TestRedactBareUserinfo(t *testing.T) {
	r := &redactor{}
	cases := map[string]string{
		// replaced: a user and a password, then a host with a dot, a port or "["
		`HTTPS_PROXY=u:hunter2@proxy.example:3128`:                            `HTTPS_PROXY=<redacted>@proxy.example:3128`,
		`pip --proxy u:hunter2@proxy.example:3128 install x`:                  `pip --proxy <redacted>@proxy.example:3128 install x`,
		`git clone u:hunter2@git.example:org/repo.git`:                        `git clone <redacted>@git.example:org/repo.git`,
		`"proxy":"carol:pa@ss@proxy.example"`:                                 `"proxy":"<redacted>@proxy.example"`,
		`curl -x dave:s3cret@192.0.2.9:8080 -I`:                               `curl -x <redacted>@<ip>:8080 -I`,
		`http_proxy=erin:pw@proxybox:3128`:                                    `http_proxy=<redacted>@proxybox:3128`,
		`proxy=(frank:pw@[2001:db8::1]:3128)`:                                 `proxy=(<redacted>@[<ip>]:3128)`,
		`fetch //u:hunter2@h.example/x`:                                       `fetch //<redacted>@h.example/x`,
		`{"url":"http:\/\/u:p@h.example\/x","next":"https:\/\/h.example\/y"}`: `{"url":"http:\/\/<redacted>@h.example\/x","next":"https:\/\/h.example\/y"}`,
		`x_https://u:p@h.example/ and y.ftp://u@h.example/`:                   `x_https://<redacted>@h.example/ and y.ftp://<redacted>@h.example/`,
		// SEC35 L-1: a user with a Windows domain, in text, JSON and a quoted
		// log value
		`HTTPS_PROXY=OTHERDOM\amy:hunter2@proxy.example:3128`:         `HTTPS_PROXY=<redacted>@proxy.example:3128`,
		`{"proxy": "CORPX\\amy:hunter2@proxy.example:3128"}`:          `{"proxy": "<redacted>@proxy.example:3128"}`,
		`err="proxy 'CORPX\\amy:hunter2@proxy.example:3128' refused"`: `err="proxy '<redacted>@proxy.example:3128' refused"`,
		`D:\lf\amy:pw@h.example x=a\b:c@h.example`:                    `D:\lf\amy:pw@h.example x=<redacted>@h.example`,
		// kept: no password, a URI scheme without "//", no host after "@"
		`contact judy@mail.example or mailto:judy@mail.example`: `contact judy@mail.example or mailto:judy@mail.example`,
		`SIP:kim@voip.example xmpp:kim@chat.example`:            `SIP:kim@voip.example xmpp:kim@chat.example`,
		`ssh kim@gpu.example and git@git.example:x.git`:         `ssh kim@gpu.example and git@git.example:x.git`,
		`image alpine:3.19@sha256:abcd`:                         `image alpine:3.19@sha256:abcd`,
		`image alpine:3.19@sha256:0123abcd`:                     `image alpine:3.19@sha256:0123abcd`,
		`at 12:30 @ noon, key:value@local`:                      `at 12:30 @ noon, key:value@local`,
		`C:\Users\<user>\a:b@c.example and /srv/a:b@c.example`:  `C:\Users\<user>\a:b@c.example and /srv/a:b@c.example`,
		`std::vector<int> v; Foo::bar@x`:                        `std::vector<int> v; Foo::bar@x`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// SEC32 L-6: a generic remote user ("root", "pi") is the user of a path in a
// remote installer's stderr: "/root/…", "/scratch/root/…", "'/root'". Any
// "/home/<name>/" is a home already. Elsewhere the word stays.
func TestRemoteRedactUserPathSegments(t *testing.T) {
	plan := []remoteRedactEntry{{ID: "zed-box", Hosts: []string{"gpu"}, Users: []string{"root", "pi"}, label: "server-1"}}
	saved := redactWordSources
	t.Cleanup(func() { redactWordSources = saved })
	redactWordSources = []func(context.Context, *Server) []redactWord{
		func(context.Context, *Server) []redactWord { return remoteRedactWords(plan) },
	}
	r := (&Server{}).redactor(context.Background())
	r.secrets, r.user = nil, nil
	cases := map[string]string{
		// replaced: a path segment of its own, the root of a path
		`detail="mkdir /root/.lectureflow/bin: Permission denied"`:    `detail="mkdir /<user>/.lectureflow/bin: Permission denied"`,
		`detail="/scratch/root/lf/kit: No space left on device"`:      `detail="/scratch/<user>/lf/kit: No space left on device"`,
		`detail="mkdir: cannot create directory '/root': Permission"`: `detail="mkdir: cannot create directory '/<user>': Permission"`,
		`detail="cd /root failed" home=/pi`:                           `detail="cd /<user> failed" home=/<user>`,
		`detail="/home/pi/.cache/uv: Read-only file system"`:          `detail="/home/<user>/.cache/uv: Read-only file system"`,
		`detail="/home/zorvex/.cache/uv and /var/home/qelt/x"`:        `detail="/home/<user>/.cache/uv and /var/home/<user>/x"`,
		// kept: the word elsewhere, inside a longer segment, a route's end
		`root cause: the root filesystem of pi`:                  `root cause: the root filesystem of pi`,
		`/usr/lib/rootless/x /opt/pi-tools/bin /srv/x.root/y`:    `/usr/lib/rootless/x /opt/pi-tools/bin /srv/x.root/y`,
		`route="POST /api/providers/{id}/root" path=/api/x/root`: `route="POST /api/providers/{id}/root" path=/api/x/root`,
		`C:\root\x and root/`:                                    `C:\root\x and root/`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// SEC34 L-3: the server of a UNC path (a data directory on a network share)
// becomes <host>, whoever it is; the DNS suffix of this machine's full name
// (its host name with dots, or USERDNSDOMAIN) becomes <domain> wherever it
// ends a host name.
func TestRedactUNCHostsAndDNSSuffix(t *testing.T) {
	r := newAccountRedactor([]string{"WS-FICTION42", "ws-fiction42.corpx.example"}, nil)
	text := []struct{ in, want string }{
		// replaced: other servers of UNC paths, in a log's two forms
		{`data_dir=\\FS01.UNI.EXAMPLE\home$\jdoe\lf`, `data_dir=\\<host>\home$\jdoe\lf`},
		{`dir="\\\\fs02\\lectures\\lf" n=1`, `dir="\\\\<host>\\lectures\\lf" n=1`},
		// (an address is left to the address rules, which run before: <ip>)
		{`(\\nas-qelt\media) and '\\192.0.2.7\x'`, `(\\<host>\media) and '\\192.0.2.7\x'`},
		{`long \\?\UNC\FS04\share\x`, `long \\?\UNC\<host>\share\x`},
		// this machine: its full name, the suffix of other names
		{`\\ws-fiction42.corpx.example\share`, `\\<host>\share`},
		{`ping WS-FICTION42.CORPX.EXAMPLE ok`, `ping <host> ok`},
		{`proxy fs03.corpx.example:8080, realm CORPX.EXAMPLE, mail a@corpx.example.`, `proxy fs03.<domain>:8080, realm <domain>, mail a@<domain>.`},
		// kept: device and long paths, drive paths, localhost, other domains
		{`\\?\C:\lf \\.\pipe\lectureflow \\localhost\c$\x`, `\\?\C:\lf \\.\pipe\lectureflow \\localhost\c$\x`},
		{`entry=D:\lf\data dir="C:\\lf\\data" x=a\\b\\c`, `entry=D:\lf\data dir="C:\\lf\\data" x=a\\b\\c`},
		{`notcorpx.example corpx.example.org corpx-example`, `notcorpx.example corpx.example.org corpx-example`},
		// SEC35 M7: a "-" before it makes it part of another name
		{`x-corpx.example and uni-corpx.example:443`, `x-corpx.example and uni-corpx.example:443`},
		// SEC35 L-2 (a): a UNC path escaped twice (Python's repr in a slog
		// quoted value), (b) a server name that is not ASCII
		{`detail="No such file or directory: '\\\\\\\\fs01\\\\share\\\\x'"`, `detail="No such file or directory: '\\\\\\\\<host>\\\\share\\\\x'"`},
		{`dir=\\文件服务器\共享\x and (\\fs-müller\share\x)`, `dir=\\<host>\共享\x and (\\<host>\share\x)`},
	}
	for _, c := range text {
		if got := r.apply(c.in, false); got != c.want {
			t.Errorf("apply(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	js := []struct{ in, want string }{
		{`{"data_dir": "\\\\FS01.UNI.EXAMPLE\\home$\\lf"}`, `{"data_dir": "\\\\<host>\\home$\\lf"}`},
		{`{"err": "No such file: '\\\\\\\\fs01\\\\share\\\\x'"}`, `{"err": "No such file: '\\\\\\\\<host>\\\\share\\\\x'"}`},
		// one backslash of JSON text: "\FS05\y", no UNC path
		{`{"x": "\\FS05\\y", "p": "C:\\lf"}`, `{"x": "\\FS05\\y", "p": "C:\\lf"}`},
	}
	for _, c := range js {
		if got := r.apply(c.in, true); got != c.want {
			t.Errorf("apply(JSON %q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	// a generic suffix (mDNS, a home network) names no one
	g := newAccountRedactor([]string{"box.local", "nas.home.arpa", "pc.lan"}, nil)
	in := `printer.local, router.home.arpa and tv.lan`
	if got := g.apply(in, false); got != in {
		t.Errorf("generic suffixes: %q", got)
	}
}

// SEC35 L-1: with the user's domain known to the account rules, the
// password of "DOMAIN\user:pass@host" goes too (it was "<domain>\
// <account-1>:hunter2@…").
func TestRedactBareUserinfoWithKnownDomain(t *testing.T) {
	t.Setenv("COMPUTERNAME", "WS-FICTION42")
	t.Setenv("USERDOMAIN", "CORPX")
	r := (&Server{}).redactor(context.Background())
	for _, in := range []string{
		`HTTPS_PROXY=CORPX\amy:hunter2@proxy.example:3128`,
		`time=2026-11-13T10:00:00Z level=WARN err="proxy 'CORPX\\amy:hunter2@proxy.example:3128'"`,
	} {
		got := string(r.apply([]byte(in)))
		if strings.Contains(got, "hunter2") || strings.Contains(got, "amy") || !strings.Contains(got, "<redacted>@proxy.example:3128") {
			t.Errorf("redact(%q) = %q", in, got)
		}
	}
	if got := string(r.applyAs([]byte(`{"proxy": "CORPX\\amy:hunter2@proxy.example:3128"}`), true)); got != `{"proxy": "<redacted>@proxy.example:3128"}` {
		t.Errorf("JSON: %q", got)
	}
}

// USERDNSDOMAIN gives this machine's full name (a domain-joined Windows).
func TestLocalAccountDomainsDNSName(t *testing.T) {
	t.Setenv("COMPUTERNAME", "WS-FICTION42")
	t.Setenv("USERDOMAIN", "CORPX")
	t.Setenv("USERDNSDOMAIN", "CORPX.EXAMPLE")
	hosts, domains := localAccountDomains()
	if !slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, "WS-FICTION42.CORPX.EXAMPLE") }) {
		t.Errorf("hosts %v lack the full name", hosts)
	}
	if !slices.Contains(domains, "CORPX") {
		t.Errorf("domains %v", domains)
	}
}
