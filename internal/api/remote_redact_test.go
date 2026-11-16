// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// SEC7 M2: the diagnostics redact network addresses, DNS names and the
// words of every SSH server ever configured (removed, edited, with the
// switch off), host names in any case; remote-servers.json numbers the
// servers.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/secrets"
	"lectureflow/internal/sshremote"
)

func TestRedactAddresses(t *testing.T) {
	r := &redactor{}
	cases := map[string]string{
		// redacted: Go's net errors, bare addresses, IPv6 in every form
		`dial tcp 192.0.2.10:22: connect: connection refused`:      `dial tcp <ip>:22: connect: connection refused`,
		`read tcp 192.0.2.77:51234->192.0.2.10:22: wsarecv: reset`: `read tcp <ip>:51234-><ip>:22: wsarecv: reset`,
		`lookup gpu on 192.0.2.53:53: no such host`:                `lookup <host> on <ip>:53: no such host`,
		`server=192.0.2.10 port=22`:                                `server=<ip> port=22`,
		`"addr":"198.51.100.7"`:                                    `"addr":"<ip>"`,
		`ssh alice@203.0.113.5`:                                    `ssh alice@<ip>`,
		`dial tcp 8.8.8.8:53: i/o timeout`:                         `dial tcp <ip>:53: i/o timeout`,
		`dial tcp 4.3.2.1 refused`:                                 `dial tcp <ip> refused`,
		`dial tcp [2001:db8::10]:22: connect: refused`:             `dial tcp [<ip>]:22: connect: refused`,
		`addr=2001:db8:0:0:0:0:0:1 x`:                              `addr=<ip> x`,
		`addr=2001:DB8::A.`:                                        `addr=<ip>.`,
		`via fe80::1%eth0`:                                         `via <ip>%eth0`,
		`mapped ::ffff:192.0.2.1 here`:                             `mapped <ip> here`,
		`http://192.0.2.10:11434/v1`:                               `http://<ip>:11434/v1`,
		// kept: loopback, unspecified, localhost
		`listening on 127.0.0.1:8765`:                `listening on 127.0.0.1:8765`,
		`listening on [::1]:8765 and localhost:8765`: `listening on [::1]:8765 and localhost:8765`,
		`bind 0.0.0.0:0 and [::]:0`:                  `bind 0.0.0.0:0 and [::]:0`,
		`127.10.0.1 ::ffff:127.0.0.1`:                `127.10.0.1 ::ffff:127.0.0.1`,
		// kept: versions, times, hashes, fingerprints that only look alike
		`LectureFlow 3.2.0.1 (go1.23.4)`:                                            `LectureFlow 3.2.0.1 (go1.23.4)`,
		`version v3.2.0.1 and V10.2.3.4`:                                            `version v3.2.0.1 and V10.2.3.4`,
		`nvidia-cublas-cu12==12.1.3.1 nvidia_cublas_cu12-12.1.3.1-py3.whl`:          `nvidia-cublas-cu12==12.1.3.1 nvidia_cublas_cu12-12.1.3.1-py3.whl`,
		`build 10.0.22631.4460 and 1.2.3.4.5 and 1234.5.6.7`:                        `build 10.0.22631.4460 and 1.2.3.4.5 and 1234.5.6.7`,
		`300.1.2.3 and 192.168.01.1`:                                                `300.1.2.3 and 192.168.01.1`,
		`time=2026-11-11T12:30:45.123+08:00 took 00:01:02.5`:                        `time=2026-11-11T12:30:45.123+08:00 took 00:01:02.5`,
		`at 12:30:45 and 1:2:3`:                                                     `at 12:30:45 and 1:2:3`,
		`mac aa:bb:cc:dd:ee:ff md5 16:27:ac:a5:76:28:2d:36:63:1b:56:4d:eb:df:a6:48`: `mac aa:bb:cc:dd:ee:ff md5 16:27:ac:a5:76:28:2d:36:63:1b:56:4d:eb:df:a6:48`,
		`SHA256:mZzVO7r/k8hniqn4pzKbobu5s0SM sha:abcdef`:                            `SHA256:mZzVO7r/k8hniqn4pzKbobu5s0SM sha:abcdef`,
		// (N31-L3: a device id of no known server becomes ssh:<server>)
		`std::string Foo::bar ssh:lab-box`: `std::string Foo::bar ssh:<server>`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// N31-L1: an IPv6 address right after "word:" is redacted (the word
// stays); what only looks alike after a word stays.
func TestRedactIPv6AfterWord(t *testing.T) {
	r := &redactor{}
	cases := map[string]string{
		`key:2001:db8::5 next`:                 `key:<ip> next`,
		`addr:fe80::1%eth0`:                    `addr:<ip>%eth0`,
		`Kind:2001:db8::a`:                     `Kind:<ip>`,
		`peer:2001:DB8:0:0:0:0:0:7.`:           `peer:<ip>.`,
		`via:2001:db8::1:22 x`:                 `via:<ip> x`,
		`from:::ffff:192.0.2.1 to:2001:db8::2`: `from:<ip> to:<ip>`,
		// kept: loopback and unspecified after a word; not addresses
		`listen:::1 bind:::`:                                  `listen:::1 bind:::`,
		`std::string Foo::bar`:                                `std::string Foo::bar`,
		`T12:30:45.123 took:00:01:02.5`:                       `T12:30:45.123 took:00:01:02.5`,
		`mac:aa:bb:cc:dd:ee:ff`:                               `mac:aa:bb:cc:dd:ee:ff`,
		`md5:16:27:ac:a5:76:28:2d:36:63:1b:56:4d:eb:df:a6:48`: `md5:16:27:ac:a5:76:28:2d:36:63:1b:56:4d:eb:df:a6:48`,
		`key:2001:db8::5gx`:                                   `key:2001:db8::5gx`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestRedactLookupHosts(t *testing.T) {
	r := &redactor{}
	cases := map[string]string{
		`dial tcp: lookup gpu7.cs.example.com: no such host`:          `dial tcp: lookup <host>: no such host`,
		`dial tcp: lookup gpu7: getaddrinfow: No such host is known.`: `dial tcp: lookup <host>: getaddrinfow: No such host is known.`,
		`dial tcp: lookup gpu7 on [::1]:53: server misbehaving`:       `dial tcp: lookup <host> on [::1]:53: server misbehaving`,
		`dial tcp: lookup GPU7.Example.COM: i/o timeout`:              `dial tcp: lookup <host>: i/o timeout`,
		`dial tcp: lookup localhost: no such host`:                    `dial tcp: lookup localhost: no such host`,
		`glossary lookup failed: the term is empty`:                   `glossary lookup failed: the term is empty`,
		`lookup table on disk`:                                        `lookup table on disk`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// The plan's words: names and slugs (any case) become the label, the
// device id its own label, hosts in any case <host>, users (case-sensitive,
// two letters too) <user>; generic names stay as words.
func TestRemoteRedactWords(t *testing.T) {
	plan := []remoteRedactEntry{
		{ID: "lab-box", Names: []string{"Lab Box"}, Hosts: []string{"gpu7.cs.example.com", "127.0.0.1"}, Users: []string{"alice", "pi"}, label: "server-1"},
		{ID: "gpu", Names: []string{"GPU"}, Hosts: []string{"192.0.2.10", "Jump.Example.com"}, Users: []string{"bob"}, label: "server-2"},
	}
	r := &redactor{}
	words := map[string]redactWord{}
	for _, w := range remoteRedactWords(plan) {
		words[asciiLower(w.word)] = w
	}
	for _, w := range words {
		r.words = append(r.words, w)
	}
	sortRedactWords(r.words)
	cases := map[string]string{
		`ssh server: not ready device=ssh:lab-box state=offline`: `ssh server: not ready device=ssh:server-1 state=offline`,
		`"name":"Lab Box" "name":"LAB BOX" id lab-box`:           `"name":"server-1" "name":"server-1" id server-1`,
		`ssh alice@gpu7.cs.example.com GPU7.CS.Example.COM`:      `ssh <user>@<host> <host>`,
		// N31-L2: "pi" is a generic user name, replaced as the user of an
		// address only
		`user pi on Pi`:                    `user pi on Pi`,
		`ssh pi@gpu7.cs.example.com`:       `ssh <user>@<host>`,
		`Alice`:                            `Alice`,
		`device=ssh:gpu uses the gpu, GPU`: `device=ssh:server-2 uses the gpu, GPU`,
		`jump JUMP.EXAMPLE.COM:22 bob`:     `jump <host>:22 <user>`,
		`tunnel 127.0.0.1:2222`:            `tunnel 127.0.0.1:2222`,
		`dial tcp 192.0.2.10:22`:           `dial tcp <ip>:22`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// N31-L2: a generic user name ("root", "pi") or single-word host name
// ("gpu", "lab") is replaced in every form of an address, and nowhere
// else; a host with a dot, an address and a user name of its own stay
// replaced everywhere.
func TestRemoteRedactGenericUserAndHost(t *testing.T) {
	plan := []remoteRedactEntry{
		{ID: "zed-box", Names: []string{"Zed Box"}, Hosts: []string{"gpu", "gpu7.cs.example.test", "192.0.2.10", "2001:db8::10"},
			Users: []string{"root", "alice"}, label: "server-1"},
		{ID: "pi-box", Names: []string{"Pi Box"}, Hosts: []string{"lab", "Jump.Example.test"}, Users: []string{"pi", "Bob"}, label: "server-2"},
	}
	s := &Server{}
	saved := redactWordSources
	t.Cleanup(func() { redactWordSources = saved })
	redactWordSources = []func(context.Context, *Server) []redactWord{
		func(context.Context, *Server) []redactWord { return remoteRedactWords(plan) },
	}
	r := s.redactor(context.Background())
	r.secrets, r.user = nil, nil // this machine's keys and user name are not part of the case
	cases := map[string]string{
		// every form of an address: the generic host and user are replaced
		`ssh root@gpu`:        `ssh <user>@<host>`,
		`ssh pi@lab -p 2222`:  `ssh <user>@<host> -p 2222`,
		`ssh://root@gpu:22/x`: `ssh://<redacted>@<host>:22/x`,
		`dial tcp gpu:22: connect: connection refused`: `dial tcp <host>:22: connect: connection refused`,
		`dial tcp [gpu]:2222`:                          `dial tcp [<host>]:2222`,
		`proxy ssh://GPU`:                              `proxy ssh://<host>`,
		`lookup gpu on 192.0.2.53:53: no such host`:    `lookup <host> on <ip>:53: no such host`,
		`dial tcp: lookup lab: no such host`:           `dial tcp: lookup <host>: no such host`,
		// SEC32 L-1: any resolver message after "lookup <host>"
		`dial tcp: lookup gpu: operation was canceled`: `dial tcp: lookup <host>: operation was canceled`,
		`Lookup LAB: lame referral`:                    `Lookup <host>: lame referral`,
		`jump pi@Lab:22`:                               `jump <user>@<host>:22`,
		// elsewhere they are the logs' own words
		`root cause: the gpu (cuda:0, gpu:0) of the lab`: `root cause: the gpu (cuda:0, gpu:0) of the lab`,
		`user root, pi and the Lab; host=gpu`:            `user root, pi and the Lab; host=gpu`,
		// a host with a dot, an address, a user of its own: everywhere
		`ssh alice@gpu7.cs.example.test`:                             `ssh <user>@<host>`,
		`alice: Permission denied; Bob too`:                          `<user>: Permission denied; <user> too`,
		`dial tcp GPU7.CS.Example.test:22 host=gpu7.cs.example.test`: `dial tcp <host>:22 host=<host>`,
		`lookup gpu7.cs.example.test: no such host`:                  `lookup <host>: no such host`,
		`jump jump.example.test via JUMP.EXAMPLE.TEST`:               `jump <host> via <host>`,
		`dial tcp 192.0.2.10:22 host=192.0.2.10`:                     `dial tcp <ip>:22 host=<ip>`,
		`dial tcp [2001:db8::10]:22 addr=2001:db8::10`:               `dial tcp [<ip>]:22 addr=<ip>`,
		`device=ssh:zed-box name="Zed Box"`:                          `device=ssh:server-1 name="server-1"`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// SEC32 L-2: user names that may be a person's ("rocky", "dev", "mac" of
// the generic server words) are not generic: replaced everywhere. "test"
// names no one (SEC32b): a user's places only, as "root".
func TestRemoteRedactUsersThatMayBeNames(t *testing.T) {
	plan := []remoteRedactEntry{{ID: "zed-box", Users: []string{"rocky", "dev", "mac", "root", "test"}, label: "server-1"}}
	forms := map[string]wordForm{}
	for _, w := range remoteRedactWords(plan) {
		forms[w.word] = w.form
	}
	for _, u := range []string{"rocky", "dev", "mac"} {
		if f, ok := forms[u]; !ok || f != 0 {
			t.Errorf("user %q: form %v (in the words: %v), want everywhere", u, f, ok)
		}
	}
	for _, u := range []string{"root", "test"} {
		if forms[u] != formUser {
			t.Errorf("user %s: form %v, want formUser", u, forms[u])
		}
	}
}

// SEC32 L-3: one word from several sources is replaced everywhere when any
// source says so (the merge in redactor), not only in the places of the
// generic one.
func TestRedactorMergeEverywhereWins(t *testing.T) {
	plan := []remoteRedactEntry{
		// "git": a generic user here, a host of its own (everywhere) there
		{ID: "zed-box", Users: []string{"git"}, label: "server-1"},
		{ID: "yak-box", Hosts: []string{"git"}, label: "server-2"},
		// "Pi": a server's name (everywhere), "pi": a generic user
		{ID: "pi-lab", Names: []string{"Pi"}, label: "server-3"},
		{ID: "wombat-box", Users: []string{"pi"}, label: "server-4"},
	}
	saved := redactWordSources
	t.Cleanup(func() { redactWordSources = saved })
	redactWordSources = []func(context.Context, *Server) []redactWord{
		func(context.Context, *Server) []redactWord { return remoteRedactWords(plan) },
	}
	r := (&Server{}).redactor(context.Background())
	r.secrets, r.user = nil, nil
	for in, leak := range map[string]string{
		`dial tcp: lookup git: operation was canceled`: "git",
		`host git port 22`:       "git",
		`server "Pi" is offline`: "Pi",
	} {
		if got := string(r.apply([]byte(in))); strings.Contains(got, leak) {
			t.Errorf("redact(%q) = %q: %q is left", in, got, leak)
		}
	}
}

// N31-L3: the device id of a server in no history (removed before the
// history existed) becomes ssh:<server>; a known server's stays numbered.
func TestRedactUnknownSSHDevices(t *testing.T) {
	plan := []remoteRedactEntry{{ID: "zed-box", Names: []string{"Zed Box"}, label: "server-1"}}
	r := &redactor{}
	for _, w := range remoteRedactWords(plan) {
		r.words = append(r.words, w)
	}
	sortRedactWords(r.words)
	cases := map[string]string{
		`device=ssh:zed-box`:                                  `device=ssh:server-1`,
		`device=ssh:old-lab-7 msg="ssh server removed"`:       `device=ssh:<server> msg="ssh server removed"`,
		`["ssh:mac-mini","cuda:0","ssh:zed-box"]`:             `["ssh:<server>","cuda:0","ssh:server-1"]`,
		`device=ssh:Old_Box2.`:                                `device=ssh:<server>.`,
		`ssh:server-1 ssh:server-12 ssh:server-x`:             `ssh:server-1 ssh:server-12 ssh:<server>`,
		`ssh: handshake failed, ssh://h.example, openssh:9.6`: `ssh: handshake failed, ssh://h.example, openssh:9.6`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// sortRedactWords orders words as redactor does (longest first).
func sortRedactWords(ws []redactWord) {
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0; j-- {
			a, b := ws[j-1].word, ws[j].word
			if len(b) > len(a) || len(a) == len(b) && b < a {
				ws[j-1], ws[j] = ws[j], ws[j-1]
			}
		}
	}
}

// A removed server, an edited host and the logs of a session with the
// switch on stay redacted in later zips, also with the switch off; the
// history itself is never in the zip.
func TestDiagnosticsRedactEveryServerEverConfigured(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	if err := secrets.SetKey(bytes.Repeat([]byte{7}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	withSSH(t, e)
	add := func(name, host, user string) sshremote.ServerView {
		res, body := e.do(http.MethodPost, "/api/remote/servers", map[string]any{"name": name, "host": host, "port": 22, "username": user})
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("add: %d %s", res.StatusCode, body)
		}
		var v sshremote.ServerView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	old := add("Alice Lab", "old-gpu.example.com", "alice")
	if res, body := e.do(http.MethodPatch, "/api/remote/servers/"+old.ID, map[string]any{"host": "gpu7.cs.example.com", "name": "Lab Seven"}); res.StatusCode != http.StatusOK {
		t.Fatalf("patch: %d %s", res.StatusCode, body)
	}
	if res, body := e.do(http.MethodDelete, "/api/remote/servers/"+old.ID, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", res.StatusCode, body)
	}
	kept := add("Zed Box", "zed.example.com", "carol")

	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	line := "level=INFO msg=\"ssh server: not ready\" device=ssh:" + old.ID + " name=\"Alice Lab\" was=\"LAB SEVEN\"" +
		" err=\"dial tcp OLD-GPU.example.com:22 and Gpu7.Cs.Example.Com via 192.0.2.10:22 lookup gpu7.cs.example.com: no such host\"" +
		" user=alice\n" +
		"level=INFO msg=\"ssh server added\" device=ssh:" + kept.ID + " host=zed.example.com user=carol\n" +
		"level=INFO msg=listening addr=127.0.0.1:8765 version=3.2.0.1\n"
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	leaks := []string{"old-gpu", "OLD-GPU", "gpu7", "Gpu7", "example.com", "192.0.2.10", "alice", "carol", "Alice Lab", "LAB SEVEN",
		"Lab Seven", "Zed Box", old.ID, kept.ID, kvRemoteRedactHistory}
	export := func(on bool) map[string]string {
		res, body := e.do(http.MethodPost, "/api/app/diagnostics", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%d %s", res.StatusCode, body)
		}
		files := unzipAll(t, body)
		for name, f := range files {
			for _, leak := range leaks {
				if strings.Contains(f, leak) {
					t.Errorf("switch on=%v: %s has %q", on, name, leak)
				}
			}
		}
		log := files["logs/lectureflow.log"]
		// the server that exists is server-1, the removed one server-2, in
		// every file
		for _, want := range []string{"device=ssh:server-2", "device=ssh:server-1", "addr=127.0.0.1:8765", "version=3.2.0.1",
			"<host>:22", "<ip>:22", "lookup <host>: no such host", "user=<user>"} {
			if !strings.Contains(log, want) {
				t.Errorf("switch on=%v: the log has no %q:\n%s", on, want, log)
			}
		}
		return files
	}
	files := export(true)
	var rs struct {
		Servers []sshremote.ServerView `json:"servers"`
	}
	if err := json.Unmarshal([]byte(files["remote-servers.json"]), &rs); err != nil || len(rs.Servers) != 1 {
		t.Fatalf("remote-servers.json: %v %s", err, files["remote-servers.json"])
	}
	if v := rs.Servers[0]; v.Name != "server-1" || v.ID != "server-1" || v.DeviceID != "ssh:server-1" {
		t.Fatalf("numbered view: %+v", v)
	}

	// the history is in this machine's database, under its kv key
	if v, ok, err := e.st.GetKV(context.Background(), kvRemoteRedactHistory); err != nil || !ok ||
		!strings.Contains(v, "old-gpu.example.com") || !strings.Contains(v, "gpu7.cs.example.com") {
		t.Fatalf("history: %q %v %v", v, ok, err)
	}

	e.sc.SetSSH(nil) // LECTUREFLOW_EXPERIMENTAL_SSH unset
	files = export(false)
	if _, ok := files["remote-servers.json"]; ok {
		t.Fatal("remote-servers.json with the switch off")
	}
}

// N31-I1: one export numbers the servers once. A server added while the
// zip is made (after the redaction's words, before remote-servers.json)
// does not renumber remote-servers.json against the logs.
func TestDiagnosticsNumberServersOncePerExport(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	if err := secrets.SetKey(bytes.Repeat([]byte{7}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	m := withSSH(t, e)
	ctx := context.Background()
	zed, err := m.Add(ctx, sshremote.AddRequest{Name: "Wombat Box", Host: "wombat.example.test", Port: 2201, Username: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	line := "level=INFO msg=\"ssh server: not ready\" device=" + zed.DeviceID + "\n"
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	// a section before remote-servers.json adds a server that sorts first
	saved := diagSections
	t.Cleanup(func() { diagSections = saved })
	added := false
	diagSections = append([]DiagSection{{Name: "add-during-export.json", Collect: func(ctx context.Context, s *Server) (any, bool) {
		if !added {
			added = true
			if _, err := m.Add(ctx, sshremote.AddRequest{Name: "Aardvark Box", Host: "aardvark.example.test", Port: 2202, Username: "dave"}); err != nil {
				t.Errorf("add during the export: %v", err)
			}
		}
		return nil, false
	}}}, saved...)

	res, body := e.do(http.MethodPost, "/api/app/diagnostics", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	if !added {
		t.Fatal("the section that adds a server did not run")
	}
	log := files["logs/lectureflow.log"]
	if !strings.Contains(log, "device=ssh:server-1\n") {
		t.Fatalf("the log: %q", log)
	}
	var rs struct {
		Servers []sshremote.ServerView `json:"servers"`
	}
	if err := json.Unmarshal([]byte(files["remote-servers.json"]), &rs); err != nil {
		t.Fatalf("remote-servers.json: %v %s", err, files["remote-servers.json"])
	}
	found := false
	for _, v := range rs.Servers {
		if v.Port == 2201 {
			found = true
			if v.ID != "server-1" || v.DeviceID != "ssh:server-1" {
				t.Errorf("the server of the log is %q / %q in remote-servers.json, ssh:server-1 in the log", v.ID, v.DeviceID)
			}
		}
	}
	if !found {
		t.Fatalf("no server on port 2201: %s", files["remote-servers.json"])
	}
	for name, f := range files {
		for _, leak := range []string{"wombat", "Wombat", "aardvark", "Aardvark", "example.test", "carol", "dave"} {
			if strings.Contains(f, leak) {
				t.Errorf("%s has %q", name, leak)
			}
		}
	}
}
