// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"io"
	"os"
	"testing"
	"time"

	"lectureflow/internal/secrets"
	"lectureflow/internal/selfupdate"
)

// -no-self-update (the desktop shell: this copy of the app cannot replace
// itself, an unpacked or copied folder) reaches the in-app update as
// selfupdate.Options.NoInstall; without it the option stays off.
func TestNoSelfUpdateFlag(t *testing.T) {
	o, err := parseArgs([]string{"-desktop", "-no-self-update"}, io.Discard)
	if err != nil || !o.NoSelfUpdate {
		t.Fatalf("%+v %v", o, err)
	}
	if o, _ = parseArgs([]string{"-desktop", "-portable"}, io.Discard); o.NoSelfUpdate {
		t.Fatal("on by default")
	}

	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-no-self-update"}, true},
		{[]string{"-portable", "-no-self-update"}, true},
	} {
		cfgPath, _ := writeTestConfig(t)
		got := make(chan selfupdate.Options, 1)
		args := append([]string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop", "-app-version", "3.2.0-test"}, tc.args...)
		p := startProc(t, args, testToken+"\n", AppOptions{SelfUpdate: func(so *selfupdate.Options) {
			offlineSelfUpdate(so)
			got <- *so
		}})
		p.ready()
		<-p.app
		select {
		case so := <-got:
			if so.NoInstall != tc.want || !so.Desktop {
				t.Errorf("%v: NoInstall = %v, want %v (desktop %v)", tc.args, so.NoInstall, tc.want, so.Desktop)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v: the in-app update was not set up", tc.args)
		}
		p.sigc <- os.Interrupt
		if c := p.wait(15 * time.Second); c != exitOK {
			t.Fatalf("%v: exit %d", tc.args, c)
		}
		secrets.ResetForTest()
	}
}
