// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// v3.2.x r5 (SEC34 信息-3, 信息-4, 信息-5): what the diagnostics replaced
// although it names no one. All names here are made up.

import (
	"testing"
)

// SEC34 信息-4: a host name that is a word of the diagnostics themselves
// ("data") is replaced in a host's places only (an address, "DATA\name",
// a UNC path); a host name of its own stays replaced everywhere.
func TestRedactHostThatIsAnOrdinaryWord(t *testing.T) {
	r := newAccountRedactor([]string{"data"}, nil)
	cases := []struct{ in, want string }{
		// kept: the logs' and the JSON files' own word
		{`the data directory, "data": 1, data_dir=x, Data folder`, `the data directory, "data": 1, data_dir=x, Data folder`},
		// replaced: the host's places
		{`DATA\amy at \\data\share`, `<host>\<account-1> at \\<host>\share`},
		{`dial tcp: lookup data: operation was canceled; ssh me@DATA:22`, `dial tcp: lookup <host>: operation was canceled; ssh me@<host>:22`},
	}
	for _, c := range cases {
		if got := r.apply(c.in, false); got != c.want {
			t.Errorf("apply(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	own := newAccountRedactor([]string{"zedkowpc"}, nil)
	if got := own.apply(`started on zedkowpc today, "zedkowpc": 1`, false); got != `started on <host> today, "<host>": 1` {
		t.Errorf("a host name of its own: %q", got)
	}
}

// SEC34 信息-5: the system's domains under their localised names (German
// Windows: "VORDEFINIERT\Benutzer") stay, as the English ones do. The
// names come from this machine's well-known SIDs (localSystemDomains).
func TestRedactLocalisedSystemDomains(t *testing.T) {
	saved := localSystemDomains
	t.Cleanup(func() { localSystemDomains = saved })
	localSystemDomains = func() []string { return []string{"VORDEFINIERT", "NT-AUTORITÄT"} }
	r := newAccountRedactor([]string{"WS-FICTION42"}, nil)
	in := `who="VORDEFINIERT\\Benutzer, NT-AUTORITÄT\\Authentifizierte Benutzer, CORPX\\amy, BUILTIN\\Users"`
	want := `who="VORDEFINIERT\\Benutzer, NT-AUTORITÄT\\Authentifizierte Benutzer, <domain>\\<account-1>, BUILTIN\\Users"`
	r.learn(in, false)
	if got := r.apply(in, false); got != want {
		t.Errorf("apply\n got %q\nwant %q", got, want)
	}
	// elsewhere too: no domain is learned from them
	in2 := `err="other accounts can still reach it (VORDEFINIERT\\Benutzer, CORPX\\amy)"`
	want2 := `err="other accounts can still reach it (VORDEFINIERT\\Benutzer, <domain>\\<account-1>)"`
	if got := r.apply(in2, false); got != want2 {
		t.Errorf("apply\n got %q\nwant %q", got, want2)
	}
}

// SEC34 信息-3: in a quoted value of the server's log (slog's text lines,
// "time=…") a backslash is "\\"; a single one starts an escape ("\n",
// "\t", "\u003e", "\"") and no account. Unquoted values and lines of
// other logs (desktop.log) keep a single backslash as a separator.
func TestRedactAccountsQuotedLogEscapes(t *testing.T) {
	r := newAccountRedactor([]string{"WS-FICTION42"}, nil)
	cases := []struct{ in, want string }{
		// kept: an escape after a host or a known domain
		{`time=2026-11-13T10:00:00Z level=INFO msg="copied to WS-FICTION42\nnext line" n=1`,
			`time=2026-11-13T10:00:00Z level=INFO msg="copied to <host>\nnext line" n=1`},
		{`time=2026-11-13T10:00:01Z level=INFO msg="say \"hi\" to WS-FICTION42\tdone" who="CORPX\\amy"`,
			`time=2026-11-13T10:00:01Z level=INFO msg="say \"hi\" to <host>\tdone" who="<domain>\\<account-1>"`},
		{`time=2026-11-13T10:00:02Z level=WARN err="open CORPX\u003e: denied"`,
			`time=2026-11-13T10:00:02Z level=WARN err="open CORPX\u003e: denied"`},
		// replaced: unquoted values, "\\" in a quoted one, other logs' lines
		{`time=2026-11-13T10:00:03Z level=WARN msg="x" who=CORPX\amy account=WS-FICTION42\qobrin`,
			`time=2026-11-13T10:00:03Z level=WARN msg="x" who=<domain>\<account-1> account=<host>\<account-2>`},
		{`time=2026-11-13T10:00:04Z level=WARN err="reach it (CORPX\\amy, Users)"`,
			`time=2026-11-13T10:00:04Z level=WARN err="reach it (<domain>\\<account-1>, Users)"`},
		{`2026-11-13T10:00:05.000Z [main] "WS-FICTION42\nora" and "CORPX\amy"`,
			`2026-11-13T10:00:05.000Z [main] "<host>\<account-3>" and "<domain>\<account-1>"`},
	}
	for _, c := range cases {
		r.learn(c.in, false)
	}
	for _, c := range cases {
		if got := r.apply(c.in, false); got != c.want {
			t.Errorf("apply(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
