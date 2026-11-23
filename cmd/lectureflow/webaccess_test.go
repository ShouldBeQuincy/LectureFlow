// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
)

func reserveFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func appendWebAccessConfig(t *testing.T, cfgPath, listen string, hosts []string) {
	t.Helper()
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	quoted := make([]string, len(hosts))
	for i, h := range hosts {
		quoted[i] = fmt.Sprintf("%q", h)
	}
	if _, err := fmt.Fprintf(f, "\n[web_access]\nlisten = %q\nhosts = [%s]\n", listen, strings.Join(quoted, ", ")); err != nil {
		t.Fatal(err)
	}
}

func TestWebAccessDesktopWiringAndShutdown(t *testing.T) {
	cfgPath, dataDir := writeTestConfig(t)
	waPort := reserveFreePort(t)
	waAddr := fmt.Sprintf("127.0.0.1:%d", waPort)
	waHost := "mymac.example-tailnet.ts.net:8443"
	appendWebAccessConfig(t, cfgPath, waAddr, []string{waHost})

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop", "-nm-endpoint", "off"}, testToken+"\n", AppOptions{})
	mainURL := p.ready()

	// Key file created in dataDir; key must NOT appear in stdout/logs.
	keyPath := filepath.Join(dataDir, api.WebAccessKeyFileName)
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" {
		t.Fatal("empty key file")
	}
	if strings.Contains(p.stdout(), key) {
		t.Fatal("web access key leaked into stdout logs")
	}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// Wrong Host -> 403
	req, _ := http.NewRequest(http.MethodGet, "http://"+waAddr+"/api/health", nil)
	req.Host = "evil.example:8443"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong host: %d", resp.StatusCode)
	}

	// No cookie -> 401
	req, _ = http.NewRequest(http.MethodGet, "http://"+waAddr+"/api/health", nil)
	req.Host = waHost
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}

	// Login -> cookie -> /api/app reports mode=web
	form := url.Values{"key": {key}}.Encode()
	req, _ = http.NewRequest(http.MethodPost, "http://"+waAddr+"/web-access/login", strings.NewReader(form))
	req.Host = waHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("login failed: %d cookies=%v", resp.StatusCode, resp.Cookies())
	}
	cookie := resp.Cookies()[0]

	req, _ = http.NewRequest(http.MethodGet, "http://"+waAddr+"/api/app", nil)
	req.Host = waHost
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("web-access /api/app: %d %s", resp.StatusCode, body)
	}
	var info api.AppInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Mode != "web" || !info.WebAccess {
		t.Fatalf("expected web mode, got %+v", info)
	}

	// Main desktop listener still requires token and reports desktop mode.
	if st := get(t, mainURL+"/api/app", nil); st != http.StatusUnauthorized {
		t.Fatalf("main listener without token: %d", st)
	}
	req, _ = http.NewRequest(http.MethodGet, mainURL+"/api/app", nil)
	req.Header.Set(api.TokenHeader, testToken)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var deskInfo api.AppInfo
	_ = json.Unmarshal(body, &deskInfo)
	if deskInfo.Mode != "desktop" || deskInfo.WebAccess {
		t.Fatalf("main listener /api/app: %+v", deskInfo)
	}

	// Graceful stop shuts down both listeners.
	p.stdinW.Close()
	if code := p.wait(10 * time.Second); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	if conn, err := net.DialTimeout("tcp", waAddr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("web access listener still open after shutdown")
	}
}

func TestWebAccessIgnoredInWebMode(t *testing.T) {
	cfgPath, dataDir := writeTestConfig(t)
	waPort := reserveFreePort(t)
	waAddr := fmt.Sprintf("127.0.0.1:%d", waPort)
	appendWebAccessConfig(t, cfgPath, waAddr, []string{"mymac.example-tailnet.ts.net:8443"})

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-nm-endpoint", "off"}, "", AppOptions{})
	_ = p.ready()

	if !strings.Contains(p.stdout(), "ignoring [web_access]") {
		t.Fatalf("expected warning in logs, got:\n%s", p.stdout())
	}
	if conn, err := net.DialTimeout("tcp", waAddr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("web access listener must not start in web mode")
	}
	if _, err := os.Stat(filepath.Join(dataDir, api.WebAccessKeyFileName)); !os.IsNotExist(err) {
		t.Fatalf("key file must not be created in web mode: err=%v", err)
	}

	p.sigc <- os.Interrupt
	if code := p.wait(10 * time.Second); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
}

func TestWebAccessBusyPortFailsStartup(t *testing.T) {
	cfgPath, _ := writeTestConfig(t)
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	waAddr := blocker.Addr().String()
	appendWebAccessConfig(t, cfgPath, waAddr, []string{"mymac.example-tailnet.ts.net:8443"})

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop", "-nm-endpoint", "off"}, testToken+"\n", AppOptions{})
	if code := p.wait(10 * time.Second); code != exitFailure {
		t.Fatalf("expected exit %d on busy port, got %d", exitFailure, code)
	}
	if !strings.Contains(p.stderr.String(), "web access listen") {
		t.Fatalf("expected clear busy-port error in stderr, got:\n%s", p.stderr.String())
	}
}
