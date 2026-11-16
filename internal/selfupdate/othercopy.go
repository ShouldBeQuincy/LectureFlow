// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"path/filepath"
	"slices"
	"time"
)

// Copies of LectureFlow sharing one data directory (r6; security review
// SEC29 section 6, case 6).
//
// The installed copy and the portable copy of one account both use
// %APPDATA%\LectureFlow unless told otherwise, and so does an unpacked
// folder. pending.json and previous.json have one slot each and do not
// say whose they are, but a package does: its kind is the one the copy
// that downloaded it installs (Windows: installer or portable). A
// pending.json of this platform whose kind is the other one is another
// copy's update, waiting for that copy's next start:
//
//   - it is not this copy's pending update: Status does not show it (it
//     would never be "installed at the next start" here), DiscardPending
//     leaves it;
//   - it stays, with its package directory and previous.json, whatever its
//     version (the other copy may be older than this one). Only one whose
//     package is gone is removed, as any other: nobody can install it;
//   - this copy starts no update of its own meanwhile: a download or an
//     offline update would end by writing previous.json and pending.json
//     over the other copy's. Both are refused with
//     selfupdate.other_copy_pending {version, kind} before anything is
//     fetched or staged, and makePending looks once more. A check still
//     runs and the newer version is still shown as available: the refusal
//     tells the user to start the other copy first.
//
// It ends when the record is gone: the other copy's shell renames
// pending.json to applying.json when it installs the update (the file is
// read again at every attempt, so no restart is needed). It also ends
// OtherPendingMaxDays after the update was verified (otherPendingGivenUp):
// the other copy may no longer exist.
//
// The desktop shell does its part (desktop/src/main/selfupdate.js
// _checkPending): it neither installs, removes nor reports such a
// pending.json.
//
// A copy that cannot update itself (an unpacked or copied folder: the
// shell finds out and says so, -no-self-update, Options.NoInstall) takes
// no package at all. Whatever pending.json it finds is another copy's,
// kept like the above; it never makes one, so nothing of it can stand in
// another copy's way: a newer version is found and shown without a
// package (the page says to download it from the releases page and
// install it by hand, and offers no download), and Download and
// ImportWith answer selfupdate.no_package.
//
// Not covered: the slots themselves stay single.

// OtherPendingMaxDays: another copy's pending update holds this copy
// back for this many days after it was verified (pending.json
// verified_at), then no longer (see otherPendingGivenUp). The texts of
// selfupdate.other_copy_pending name the number.
const OtherPendingMaxDays = 14

// otherCopys reports whether pending.json p (its platform and kind) is the
// update of another copy of LectureFlow than this one: for a copy that
// cannot update itself (Options.NoInstall) every one; else on Windows the
// package of the other kind of copy (installer / portable). Nothing else
// is: on macOS the zip and the dmg are both this installation's, and a
// platform or kind no copy here takes is not installable at all. The same
// rule as the desktop shell's (selfupdate.js otherCopysPending); a table
// in both tests keeps them together.
func (u *Updater) otherCopys(p *Pending) bool {
	if u.opt.NoInstall {
		return true // this copy installs none: whatever waits there is another copy's
	}
	if u.platform != PlatformWindowsX64 || p.Platform != u.platform {
		return false
	}
	if p.Kind == u.kind || slices.Contains(u.importKinds(), p.Kind) {
		return false
	}
	return p.Kind == KindInstaller || p.Kind == KindPortable
}

// otherPendingGivenUp reports whether another copy's pending update p no
// longer holds this copy back: it was verified more than
// OtherPendingMaxDays ago, so the copy it belongs to has not been started
// since and may be gone (a portable copy that was tried and deleted would
// otherwise keep this one from updating for ever; lead's ruling, SEC40).
// One "verified" more than otherPendingClockSlack from now is given up
// as well: it was made while the clock was set far ahead, and would
// otherwise hold this copy back until that day (SEC40b (c)); a clock that
// is a little off is no reason. Without a readable verified_at it is not
// given up. A given-up update is still not this copy's and is not removed
// for its age: the next download or offline update takes the slot, as it
// always did, and if the other copy does come back it downloads again.
func (u *Updater) otherPendingGivenUp(p *Pending) bool {
	at, err := time.Parse(time.RFC3339, p.VerifiedAt)
	if err != nil {
		return false
	}
	age := u.now().Sub(at)
	return age > OtherPendingMaxDays*24*time.Hour || age < -otherPendingClockSlack
}

// otherPendingClockSlack: how far ahead of this computer's clock another
// copy's verified_at may be and still be believed.
const otherPendingClockSlack = 24 * time.Hour

// otherCopyPending returns selfupdate.other_copy_pending {version, kind}
// while pending.json is another copy's update that is still waited for
// (see above), nil otherwise. The file is read each time; another copy's
// update it finds, given up or not, is also what cleanup keeps.
func (u *Updater) otherCopyPending() error {
	if u.reason != "" || u.dir == "" {
		return nil
	}
	var p Pending
	ok, err := readJSON(filepath.Join(u.dir, pendingFile), &p)
	theirs := ok && err == nil && p.Schema == fileSchema && validVersion(p.Version) && u.otherCopys(&p)
	u.mu.Lock()
	if theirs {
		u.other = &p
	} else {
		u.other = nil
	}
	u.mu.Unlock()
	if !theirs || u.opt.NoInstall {
		// (a copy that cannot update itself starts no update that could
		// replace it: its downloads and offline updates end in no_package)
		return nil
	}
	if u.otherPendingGivenUp(&p) {
		u.log.Info("selfupdate: another copy's pending update was not installed for more than 14 days (or is dated more than a day ahead); this copy no longer waits for it",
			"version", p.Version, "kind", p.Kind, "verified_at", p.VerifiedAt)
		return nil
	}
	return cerr(CodeOtherCopyPending, map[string]any{"version": p.Version, "kind": copyParam(p.Kind)}, nil)
}
