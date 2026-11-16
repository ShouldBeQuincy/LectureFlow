// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package api

import (
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// systemDomainSIDs are well-known SIDs, one for each of the system's
// domains (redact_account.go systemDomains): Windows gives their domain in
// its own language ("VORDEFINIERT" for BUILTIN on a German system). A SID
// this machine does not know (no Hyper-V, no IIS) is skipped.
var systemDomainSIDs = []string{
	"S-1-5-32-544", // BUILTIN\Administrators
	"S-1-5-18",     // NT AUTHORITY\SYSTEM
	"S-1-5-80-0",   // NT SERVICE\ALL SERVICES
	"S-1-5-83-0",   // NT VIRTUAL MACHINE\Virtual Machines
	"S-1-15-2-1",   // APPLICATION PACKAGE AUTHORITY\ALL APPLICATION PACKAGES
	"S-1-5-90-0",   // Window Manager\Window Manager Group
	"S-1-5-96-0",   // Font Driver Host\…
}

var systemDomainsOnce = sync.OnceValue(func() []string {
	done := make(chan []string, 1)
	go func() {
		var out []string
		for _, s := range systemDomainSIDs {
			sid, err := windows.StringToSid(s)
			if err != nil {
				continue
			}
			if _, dom, _, err := sid.LookupAccount(""); err == nil && dom != "" {
				out = append(out, dom)
			}
		}
		done <- out
	}()
	// well-known SIDs resolve on this machine; a lookup that hangs anyway
	// leaves the English names only
	select {
	case out := <-done:
		return out
	case <-time.After(2 * time.Second):
		return nil
	}
})

func systemDomainsOfSIDs() []string { return systemDomainsOnce() }
