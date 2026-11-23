// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebAccessConfigOffByDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WebAccess.Enabled() || c.WebAccess.Listen != "" || len(c.WebAccess.Hosts) != 0 {
		t.Fatalf("expected web_access off by default, got %+v", c.WebAccess)
	}
}

func TestWebAccessConfigValid(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{
			name: "127.0.0.1 with port host",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
hosts = ["mymac.example-tailnet.ts.net:8443"]
`,
		},
		{
			name: "localhost with bare and port hosts",
			toml: `
listen = "127.0.0.1:0"
[web_access]
listen = "localhost:8421"
hosts = ["mymac.example-tailnet.ts.net", "mymac.example-tailnet.ts.net:8443"]
`,
		},
		{
			name: "empty section is off",
			toml: `
[web_access]
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.toml")
			if err := os.WriteFile(p, []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.name != "empty section is off" && !c.WebAccess.Enabled() {
				t.Fatalf("expected Enabled()=true")
			}
		})
	}
}

func TestWebAccessConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{
			name: "non-loopback listen",
			toml: `
[web_access]
listen = "0.0.0.0:8421"
hosts = ["mac.ts.net:8443"]
`,
		},
		{
			name: "port zero",
			toml: `
[web_access]
listen = "127.0.0.1:0"
hosts = ["mac.ts.net:8443"]
`,
		},
		{
			name: "non-numeric port",
			toml: `
[web_access]
listen = "127.0.0.1:abc"
hosts = ["mac.ts.net:8443"]
`,
		},
		{
			name: "same as main listen",
			toml: `
listen = "127.0.0.1:8420"
[web_access]
listen = "127.0.0.1:8420"
hosts = ["mac.ts.net:8443"]
`,
		},
		{
			name: "same port as main listen via localhost",
			toml: `
listen = "127.0.0.1:8420"
[web_access]
listen = "localhost:8420"
hosts = ["mac.ts.net:8443"]
`,
		},
		{
			name: "missing hosts",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
`,
		},
		{
			name: "empty hosts",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
hosts = []
`,
		},
		{
			name: "host with scheme",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
hosts = ["https://mac.ts.net:8443"]
`,
		},
		{
			name: "host with path",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
hosts = ["mac.ts.net:8443/app"]
`,
		},
		{
			name: "host with whitespace",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
hosts = ["mac.ts.net:8443 "]
`,
		},
		{
			name: "empty host entry",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
hosts = [""]
`,
		},
		{
			name: "host with invalid port",
			toml: `
[web_access]
listen = "127.0.0.1:8421"
hosts = ["mac.ts.net:0"]
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.toml")
			if err := os.WriteFile(p, []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}
