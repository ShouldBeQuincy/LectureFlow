# In-app update: update.json, signatures and the contract with the desktop shell

This package is the Go side of the in-app update (SPEC-ext "应用内更新（不依赖自有服务器）").
It needs no server of our own: releases live on GitHub Releases, every release
carries a signed `update.json`, and the application trusts nothing but the
minisign public key compiled into it. Mirrors may therefore be added freely:
a mirror that changes a single byte is refused.

What the Go service does: check (once a day after a check that worked, sooner
after a failed one; can be switched off),
download this installation's package, accept offline updates from files, verify
everything, and leave the verified package in `{data_dir}/updates/` with a
`pending.json`. What the desktop shell (Electron) does: at the next start,
before the service starts, install the pending package, keep the previous
version, and go back to it when the new one fails. Electron's side is
described in "The updates directory" below.

This document is the **frozen** description of schema 1. Changes that old
versions could misread need a new `schema` number (old versions then report
`selfupdate.manifest_invalid {field: "schema"}` and the user installs by
hand).

## update.json (schema 1)

```json
{
  "schema": 1,
  "app": "LectureFlow",
  "version": "3.2.0",
  "channel": "stable",
  "released_at": "2026-11-02T10:00:00Z",
  "min_version": "3.0.0",
  "notes_url": "https://github.com/OWNER/REPO/releases/tag/v3.2.0",
  "platforms": {
    "windows-x64": [
      {
        "kind": "installer",
        "name": "LectureFlow-Setup-3.2.0-x64.exe",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-Setup-3.2.0-x64.exe",
        "size": 189123456,
        "sha256": "3f1d0c8a3e5b0e7f2a9c4d6b8e1f3a5c7e9b0d2f4a6c8e0b2d4f6a8c0e2b4d6f"
      },
      {
        "kind": "portable",
        "name": "LectureFlow-3.2.0-x64-portable.exe",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-3.2.0-x64-portable.exe",
        "size": 188987654,
        "sha256": "8b2e4f6a0c2e4a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c"
      }
    ],
    "darwin-arm64": [
      {
        "kind": "dmg",
        "name": "LectureFlow-3.2.0-arm64.dmg",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-3.2.0-arm64.dmg",
        "size": 201234567,
        "sha256": "0c2e4a6c8e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c6e8b0d2f"
      },
      {
        "kind": "zip",
        "name": "LectureFlow-3.2.0-arm64-mac.zip",
        "url": "https://github.com/OWNER/REPO/releases/download/v3.2.0/LectureFlow-3.2.0-arm64-mac.zip",
        "size": 198765432,
        "sha256": "e0b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2c4e6b8d0f2a4c6e8b0d2f4a6c8e0b2"
      }
    ]
  },
  "mirrors": [
    {"prefix": "https://github.com/", "replace": "https://mirror.example.org/github/"}
  ]
}
```

| Field | Rule |
|---|---|
| `schema` | `1`. Any other value is refused. |
| `app` | `"LectureFlow"`. |
| `version` | Semantic version `MAJOR.MINOR.PATCH[-pre][+build]`, no `v`. Must equal the version in the signature's trusted comment. |
| `channel` | `stable` or `beta`. A `stable` release has no pre-release part. Stable users are only offered stable releases; beta users both. |
| `released_at` | RFC 3339. |
| `min_version` | Optional. Installations older than this cannot update directly: the release is shown (`available.direct = false`) but not downloaded; the user installs it by hand. Not above `version`. |
| `notes_url` | Optional, https. |
| `platforms` | Platform → packages. Platforms of 3.x: `windows-x64`, `darwin-arm64`. Each package: `kind` (lower case, `[a-z0-9][a-z0-9:_-]*`, unique per platform), `name` (a plain file name: no path, no leading dot, no `..`), `url` (http or https, no user info; packages are only downloaded over https, or http to this computer), `size` (1 byte to 4 GiB), `sha256` (64 lower-case hex digits). |
| `mirrors` | Optional. Every package URL that starts with `prefix` (ending in `/`) can also be fetched from `replace` + the rest of the URL. |

Kinds of 3.x: `installer` (Windows NSIS installer, per user), `portable` (Windows
portable executable), `dmg` (macOS disk image), `zip` (macOS: the `.app` in a
zip, what downloaded updates use). The installation picks its kind: Windows
installed → `installer`, Windows portable (`-portable`) → `portable`, macOS →
`zip` (an offline update may also bring the `dmg`).

Forward compatibility: unknown top-level fields, unknown platforms and unknown
kinds are ignored (but every listed package must still be well-formed).
Components stay in the component manifest shipped with each version
(`packaging/components/`); should update.json ever list them, it adds packages
of a new kind (for example `component:asr-cuda`), which 3.x ignores.

## Signature (minisign)

`update.json.minisig` is a [minisign](https://jedisct1.github.io/minisign/)
signature of the exact bytes of `update.json`, made by
`scripts/release/sign-update.{ps1,sh}` on the maintainer's computer:

```
untrusted comment: <anything; not signed>
<base64: "ED" (or legacy "Ed") | key id (8 bytes) | Ed25519 signature (64 bytes)>
trusted comment: lectureflow update version:3.2.0
<base64: global signature (64 bytes) over signature || trusted comment>
```

- Both signature kinds are accepted: `ED` (the default since minisign 0.11:
  Ed25519 over the BLAKE2b-512 hash of the file) and the legacy `Ed` (Ed25519
  over the file itself).
- The **trusted comment** is `lectureflow update` followed by space-separated
  `key:value` fields, exactly one of them `version:<version>`; other fields
  are allowed and ignored. minisign's default trusted comment
  (`timestamp:… file:…`) is refused.
- The public key is the constant `PublicKey` in `keys.go` (the base64 line of
  the `.pub` file). Since 2026-10-27 (3.2.0) it is the maintainer's key
  `RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX`, key id
  `0FAC4F1B6C17636D` as minisign prints it (`PublicKeyID`; changing it:
  scripts/release/README.md, "Changing the key"). **A build without a key
  is "not configured"** (`PublicKey` empty, or built with `-ldflags "-X
  lectureflow/internal/selfupdate.builtinPublicKey="`): the status says
  `not_configured` (`reason: no_public_key`), nothing is checked or
  downloaded, no error is shown, and the desktop shell keeps the older
  electron-updater path. `Repo` (`owner/name`) names the GitHub repository;
  while it is empty only the extra sources of the settings are asked.
- The secret key is a password-protected minisign key file on the maintainer's
  computer only. CI never holds it: CI builds an unsigned `update.json`, the
  maintainer signs it and uploads the `.minisig` (scripts/release/README.md).

Verification order (any failure refuses the whole update.json):

1. the `.minisig` parses (at most 16 KiB) and its key id is the built-in key's
   (`selfupdate.signature_invalid`);
2. the signature verifies over the raw bytes, then the global signature over
   the trusted comment (`selfupdate.signature_invalid`);
3. only now update.json is parsed and validated (`selfupdate.manifest_invalid {field}`);
4. the trusted comment's version equals `version` (`selfupdate.version_mismatch {signed, manifest}`);
5. **no downgrade**: `version` must be newer than the running version. A
   check that finds nothing newer reports `up_to_date`; an offline update
   that is not newer is refused (`selfupdate.not_newer {version, current}`); a
   `pending.json` that is not newer than the running version is dropped at
   start;
6. a package is accepted only when its size and SHA-256 equal the entry of
   update.json (`selfupdate.checksum {file}`).

Nothing is downloaded into, imported into, installed from or rolled back
from a data directory that other accounts of the computer can change (the
data directory or its `updates/` directory: a foreign owner, or an allow
entry with write rights for anyone but the user, SYSTEM, Administrators,
TrustedInstaller; on macOS / Linux group or others write access on a path
others can reach): download and import answer 409
`selfupdate.untrusted_dir {path, who}`, `-selfupdate-verify` exits 3. The
diagnostics page warns about such a data directory and offers to make it
private (`POST /api/system/data-dir/restrict`); LectureFlow makes the data
directories it creates or moves private from the start (internal/dirperm).

The same holds for what lies below `updates/` (security audit M-3): a
folder another account made there while it could write stays that
account's, whatever the data directory's permissions are now. `CheckTrust`
therefore looks at every entry below `updates/` (`dirperm.CheckTree`: the
entry's own owner and permissions — on Windows every entry of the DACL
that is not a deny entry counts, object and callback kinds included, and
writing attributes is a write right —, links never followed, at most
50 000 entries and 32 levels) and, when the desktop shell's profile lies in
the data directory, at `electron/` and `electron/selfupdate-trust.json`
(the shell's record of the backups' hashes). The diagnostics look at more
(the component environments, the whole profile) and end with this very
check, so they never call private what the update refuses. `path` is then the entry found; a
tree that cannot be vouched for (a link on Windows, more entries than the
bound, an entry that cannot be examined) is refused with `who` = `not
confirmed: <why>`. "Restrict access" on the diagnostics page resets such
entries or says which one it could not reset.

Permissions say who can change the files from now on, not who wrote them.
What a rollback believes — `applying.json`, the shell's record in
`electron/selfupdate-trust.json`, the backups, previous.json's `backup` —
can be prepared as a whole in a data directory that holds the shell's
profile while other accounts can write there (security review SEC17). So
when "Restrict access" finds that the data directory was open to change
(before the click, or an entry below it had to be reset, or the last look
at it had not vouched for it), the service removes exactly that state
(`VoidRollback`: the record file, `applying.json`, `updates/backup/`,
previous.json's `backup`) and the answer carries `"rollback_reset": true`
(next to `"error"` too, when the request then fails for another reason).
Nothing is followed out of the data directory: `updates/` and `electron/`
are entered only as plain directories, through handles (`os.Root`); one
that is a junction, a symbolic link or another reparse point is removed
itself, never what it leads to, and so is such a link among the files or
below `backup/`. The update's own clean-up of old version folders
likewise removes nothing through an `updates/` that is a link, and the
shell's pruning of backups does not follow one either. The shell then
finds no record: it ignores an `applying.json`, rolls
nothing back and runs nothing. An update that really was on probation
stays installed; should it fail to start later, the error of the start is
shown and the previous version is not put back. The next update backs the
installation up again. A data directory that was private all along, or
whose shell profile lies under the user's own profile folder (a moved data
directory), keeps its rollback state. While others can change the data
directory nothing needs removing: every rollback is refused (exit 3).

### The shell's record outside the data directory (Windows)

Removing the state when the user restricts the folder protects only
against what is still visible then: an account that once could write to
the data directory can prepare the whole rollback state, record included,
and then remove every trace of itself, so that nothing looks wrong and the
button never appears (security review SEC17, 中-B). On Windows the shell
therefore keeps its record — the installation it started, the backups'
hashes — in `%LOCALAPPDATA%\LectureFlow\selfupdate\trust.json` (installed
and portable builds alike; not moved with the data, not affected by
`LECTUREFLOW_DATA_DIR`), and nothing in the data directory counts without
it.

- Every `-selfupdate-verify` run of the shell names that folder
  (`-selfupdate-trust-dir`). `CheckTrustDir` makes it privately when it is
  missing and refuses it (exit 3, like an untrusted data directory) when
  it lies in the data directory or the data directory lies in it (also
  with links resolved), when it is a link, or when another account can
  change it or anything in it. `-selfupdate-trust-check` checks only that
  (`ok trusted`).
- Taking over the profile's record of an older version happens once:
  `migrated.json` in the new folder (never a mark in the profile) is
  written first, and the profile is never read again. Only the
  installation now on probation is taken over (applying.json "installing"
  of the running version, not started yet — `tries` 0 or missing, as the
  older shell writes it — the same versions and target as the old record,
  the target being this installation, the service program vouching for
  both folders); the record of its backup with it when the profile does
  not lie in the data directory, or when the data directory is the default
  one, `%APPDATA%\LectureFlow` of this account (`APPDATA` an absolute path
  with a drive letter; compared without regard to case or a final
  separator), with the profile in it and no link, short name or
  substituted drive on the way (SEC17 中-H, option b, approved by the user
  2026-11-11: otherwise the first update after the move could not go back
  for most users). A backup record taken over is used like any other: the
  service program is asked about the previous version, the backup is
  copied into a folder it made, and the copy is used only with the
  recorded hash. Anywhere else the worst a prepared old record can do is a
  rollback to a previous version's package verified with the built-in key
  (an older signed version); no backup from the data directory is used.
  Known limitation: in the default data directory, an account that could
  write there before this version's first start and removed every trace
  of itself (so the check passes) can have its own program installed in
  place of LectureFlow and started, as the user, if the new version then
  fails to start twice (the checks use the hash in that account's own
  record, so they do not stop it); this one move only, accepted by the
  user.
- The shell drops its record of an installation when there is no
  applying.json (a crash, or "Restrict access" removed it), so that a
  later applying.json cannot match it.
- `VoidRollback` keeps acting as above (defence in depth): it removes the
  profile's old record, never the new one.
- The NSIS uninstaller removes the folder, except during an update.
- macOS keeps the record in the profile (there is no Mac to test a move
  on): a known limitation. Linux has no in-app update.

### The installation directory (Windows)

The installer installs per user, without administrator rights, wherever
the user chooses (security audit M-4). The default,
`%LOCALAPPDATA%\Programs\lectureflow`, admits only the user, SYSTEM and
the administrators; outside the user's profile and `%LOCALAPPDATA%` the
installer gives the folder the same private permissions
(`desktop/packaging/installer.nsh`, `icacls … /inheritance:r /grant:r`).
A folder whose permissions let other accounts change it all the same (made
by an older version in `D:\`, changed since, on FAT / exFAT, or below a
folder others may rename) is not repaired by the app; it is refused:

- The shell names the installation directory only for the Windows
  installation set up by the installer (`updatefiles.installDirOf`):
  development runs, the portable build and macOS have none, and nothing is
  checked for them.
- `CheckInstallDir` (installdir.go) refuses it as
  `selfupdate.untrusted_install_dir {path, who}` when it is not absolute or
  is a link, when another account owns it or may change it
  (`dirperm.Check`), may remove or rename it or a folder above it
  (`dirperm.CheckAbove`: DELETE, FILE_DELETE_CHILD or the rights to give
  itself those, on entries that apply to that folder itself; the root of
  the drive only for what lets others remove its entries), or owns or may
  change an entry below it (`dirperm.CheckTree`, at most 20 000 entries and
  32 levels, links never followed; `who` = `not confirmed: <why>` when the
  walk cannot vouch for everything). SYSTEM, the administrators and
  TrustedInstaller count as the user (Program Files passes); reading
  counts for nothing, and `who` names only accounts that may change
  something. A folder on the way that is a link (junction, symbolic link)
  is not vouched for (`not confirmed`), and the path the system really
  opens (`dirperm.FinalPath`: subst and mapped drive letters, links) is
  walked up as well. A check that fails (the folder cannot be read) is
  `not confirmed` too: refused like the rest (security review SEC20).
- The service gets it as `-install-dir`: download and import answer 409
  `selfupdate.untrusted_install_dir` (after the data directory's
  `untrusted_dir`); the diagnostics show the `install_dir` row (a warning
  with a hint that fits where the problem lies: the folder itself, a folder
  above it, or the default place inside the profile, where reinstalling
  does not help; no button).
- Every `-selfupdate-verify` run of the shell names it
  (`-selfupdate-install-dir`), checked after the data directory and the
  trust folder, in every mode: exit 3 with the line
  `untrusted_install_dir: …`. Nothing is installed or rolled back;
  `pending.json` is kept, so the update installs once the folder is fixed.
- After a rollback from a backup (`_replaceDir`: the failed installation
  renamed away, the backup copied into a new folder of the same name) the
  restore program asks for `-selfupdate-trust-check` with
  `-selfupdate-install-dir` set to the installation it restored (applying.json's
  `target`, not its own folder) and adds what it does not vouch for to the
  rollback's reason (`…; after going back: …`); the rollback and the start
  of the restored version are not affected (SEC20 中-1, 低-A).
- An installation "for all users" (an administrator running the installer,
  or `/allusers`: Program Files) keeps its permissions (SEC20 中-3).

Known limitations (SEC20 中-2): the installer tightens the folder only
after the files were unpacked, and during an in-app update the old
version's uninstaller removes the folder and the new installer makes it
again — with its parent's permissions until it is tightened. Another
account that watches the path at that moment can change files while they
are written (the check afterwards sees the user's files with private
permissions) or take the folder's name first (then it is its owner, which
the check does report). Closing this needs the installer to make the
folder privately before unpacking and the update to keep it
(electron-builder's `customUnInstallCheck` / `customRemoveFiles`); not done
without a way to run the installer here. A folder of that name another
account made before the first installation falls under the same
limitation: the installer tightens it only after unpacking into it. After
a failed update is rolled back, the installation folder is made anew and
takes its permissions from the folder above it: outside the default place
the private permissions do not come back by themselves — the app checks
the folder and says so, and the user installs again or tightens it. In
that case, while the rollback copies the files (after the failed folder
is renamed away and before the copy is complete), the folder and the
files being copied can be changed by the other accounts that may change
the folder above; the check afterwards sees wrong permissions, not
changes made in that time. The
check runs the service
program from the installation folder itself: it reports a folder that is
open to others, not one whose programs others already replaced.
electron-updater (builds without the update key) does not go through it.
macOS is not checked (no Mac to test on).

## Sources and mirrors

update.json is asked from every source below at the same time; of the
update.json files that verify, the **newest one the channel may offer**
wins (the first source in this order among equal versions). A source that
serves an old release, signed but stale, cannot hide a newer one another
source has. Sources are https; plain http is accepted only for this
computer (`127.0.0.1`, `::1`, `localhost`: a local cache), since on the way
anyone could see what is fetched and hold back newer releases.

1. GitHub (when `Repo` is set). Stable channel:
   `https://github.com/<repo>/releases/latest/download/update.json` and
   `….minisig` (GitHub's "latest" never points at a pre-release or a draft).
   Beta channel: first the public releases API
   (`https://api.github.com/repos/<repo>/releases?per_page=20`): among the
   published releases with both `update.json` and `update.json.minisig` assets
   (asset URLs must be this repository's `releases/download/` URLs), the one
   with the highest tag version; then the stable URL.
2. The extra sources of the settings (`selfupdate.sources`, at most 8), in
   order (https; a stored entry of an older version that is not is left
   out). Each is one of:
   - a proxy template with `{url}`: `https://proxy.example/{url}`; `{url}` is
     replaced with the GitHub URL, for update.json, its signature and the
     packages;
   - a URL ending in `.json`: that update.json, its signature at the same URL
     plus `.minisig`, the packages in the same directory;
   - any other https URL: a directory holding `update.json`,
     `update.json.minisig` and the packages under their `name`.

The channel is the existing setting `app.update_channel`.

Packages are downloaded from, in order: the source that delivered update.json,
the package's `url`, the other extra sources, then update.json's `mirrors`
rules; a URL that is plain http to another computer is skipped. Every
candidate serves the same bytes or is refused by the SHA-256.

**No expiry.** The signature carries no time, and update.json's
`released_at` is not compared with the clock: releases are signed by hand on
the maintainer's computer, not on a schedule, and an offline update may
rightly bring a release from months ago. A source that is stuck on an old
release can only withhold the newer one (never install anything unsigned or
older than the running version), and asking every source makes that
need all of them.

**Privacy.** Requests carry `User-Agent: LectureFlow` and nothing else that
could identify the user: no cookies (no cookie jar; `Cookie` is dropped on
redirects), no `Referer` (dropped on redirects), no installation id, no version
in the URL. The releases API request adds `Accept: application/vnd.github+json`.
Redirects are followed only to https, or to plain http on this computer, and
never from https down to http (also on the same host); the bundled aria2c
cannot be restricted like this (the signature and SHA-256 still decide).
Automatic checks run a day after a check that worked; after a failed one
sooner, backing off (`CheckInterval`): 1 h, 3 h, 6 h, then a day; while the
latest release has no update.json or signature (`no_manifest`) every 2 h for
about a day (12 times), then every 6 h, so that an update is picked up within
hours of the maintainer uploading the signature. A check by hand counts too;
the time, the failures in a row and the last failure are kept in kv
`selfupdate.last_check` (the status shows the failure again after a restart,
and the back-off goes on). A last check "in the future" (the clock was set
back) counts as made now. Automatic checks can be switched off
(`selfupdate.auto_check`). Nothing is downloaded automatically: the user starts
the download.

## Downloading

`Downloader` is the interface the application implements with the bundled
aria2c (internal/fetch); `HTTPDownloader` is the net/http fallback. Contract
(also in `download.go`):

- write to the given path (`updates/<version>/<name>.part`), resuming what is
  there; helper files must start with that path (`….part.aria2`) and are
  removed when done;
- the URLs are mirrors of one file, best first; `UserURLs` names those that
  come from the user's own sources (a directory or a `{url}` template), whose
  hosts alone may be on this computer or the local network (the aria2c
  downloader's guard proxy judges every other URL like a recording's);
- send only `User-Agent: LectureFlow`;
- return nil once the file has the expected size; the updater then hashes it,
  deletes it on a mismatch (`selfupdate.checksum`), and renames it to
  `updates/<version>/<name>`;
- stop promptly when the context ends (cancel = `DELETE /api/selfupdate/download`;
  the partial file stays for the next attempt).

## Offline update ("从文件更新")

`POST /api/selfupdate/import` takes the files the user picked or dropped:
`update.json`, `update.json.minisig` and the package, in any order and under
any names (they are told apart by content), or **one zip** holding the three
(anywhere in the zip; `__MACOSX/` and dot files are ignored; the package is the
entry named like this installation's package, else the only other file). The
macOS package is itself a zip; given alone it is a package, not a bundle
(`selfupdate.import_incomplete {missing: "manifest"}`).

The user's files are copied once into `updates/.import-*/` (removed afterwards)
and everything else reads only that copy. The checks are those of a download,
without any network: signature, version binding, newer than the running
version, `min_version`, and a package of this platform whose size and SHA-256
match (on macOS the `zip` or the `dmg`). The channel is not checked: importing
a beta by hand is the user's choice.

## The updates directory (contract with Electron)

```
{data_dir}/updates/
  <version>/update.json            verified signature and manifest (Go)
  <version>/update.json.minisig
  <version>/<package name>         verified package (Go)
  <version>/<package name>.part    a download in progress (Go)
  pending.json                     Go → Electron: install this at the next start
  previous.json                    Go (Electron may add "backup"): what a failed update returns to
  result.json                      Electron → Go: the outcome of the last installation
  latest.json, latest.json.minisig the last verified check result (Go)
  .import-*/                       offline-update staging (Go)
  backup/                          Electron's copies of replaced installations (Go never touches it)
```

All JSON files are UTF-8, written to a temporary file in the same directory and
renamed over the target. Paths inside them are **relative to the updates
directory, with `/`** (the data directory can be moved).

### pending.json (written by Go; read and consumed by Electron)

Written only after every check passed (signature with the built-in key, version
binding, newer than `from_version`, size and SHA-256 of the package).

```json
{
  "schema": 1,
  "version": "3.2.0",
  "from_version": "3.1.0",
  "channel": "stable",
  "platform": "windows-x64",
  "kind": "installer",
  "file": "3.2.0/LectureFlow-Setup-3.2.0-x64.exe",
  "size": 189123456,
  "sha256": "3f1d0c8a3e5b0e7f2a9c4d6b8e1f3a5c7e9b0d2f4a6c8e0b2d4f6a8c0e2b4d6f",
  "manifest": "3.2.0/update.json",
  "signature": "3.2.0/update.json.minisig",
  "key_id": "8F2D0A6B3C1E4D57",
  "origin": "download",
  "verified_at": "2026-11-02T12:00:00Z"
}
```

`origin` is `download` or `file` (offline update). `key_id` is the key id as
minisign prints it.

### previous.json (written by Go together with pending.json)

Describes the running version, to return to when the update fails. Written
before pending.json.

```json
{
  "schema": 1,
  "version": "3.1.0",
  "platform": "windows-x64",
  "kind": "installer",
  "file": "3.1.0/LectureFlow-Setup-3.1.0-x64.exe",
  "size": 188000000,
  "sha256": "…",
  "backup": "backup/3.1.0",
  "created_at": "2026-11-02T12:00:00Z"
}
```

`file` is set when `updates/<running version>/` holds that version's verified
package (it was installed through the updater); otherwise it is absent and
Electron must back up the installation before replacing it, then rewrite
previous.json with `backup` (the only field Electron may change; keep the
others). Go keeps `backup` when it rewrites previous.json for the same version.

### result.json (written by Electron)

```json
{"version": "3.2.0", "from": "3.1.0", "outcome": "installed", "at": "2026-11-03T08:00:00Z"}
```

`outcome`: `installed`, `rolled_back` (the new version failed and the previous
one was restored) or `failed` (nothing was changed); `error`: optional short
English reason (shown under "查看详情"). Go only reads it (status
`last_result`).

**A failed installation is remembered.** When result.json says `rolled_back`
or `failed` for a version newer than the running one and there is no
pending update of it (Electron keeps `pending.json` when it only could not
check the package at this start: no failure), its `from` is the running
version and `updates/<version>/` holds that version's update.json with a
signature that verifies with the built-in key (a planted or stale
result.json cannot hold back an update), Go records it at its start in
kv `selfupdate.failed_install` (`{version, at, outcome, error, retry}`; `at`
is result.json's, so the same result is recorded once). Until a strictly
newer version appears (offered by a check, pending, or the running version
itself), that version is not offered again (status `install_failed`) and its
`updates/<version>/` directory is removed by the start's clean-up (an entry
of the updates directory whose name is a version; never a path taken from a
JSON file). Downloading it again, or an offline update of it, is the user's
"try again": the record gets `retry` (no longer in effect, and the same
result.json is not recorded again); a new failure is recorded again.

### What Electron does at start (before starting the service)

1. Read `pending.json`. Ignore it (and delete it) unless `schema` is 1,
   `platform` and `kind` are this installation's, `version` is newer than the
   running version, and the file at `file` has `size` bytes and the SHA-256
   `sha256` (hash it again: the file may have been damaged since). One that
   is another copy's is ignored but **not** deleted or reported ("Copies
   sharing one data directory" below).
   Then run the installed (old) service program to check it all again with
   its built-in public key, signature included, and to copy the package
   where nothing else can change it:
   `lectureflow -selfupdate-verify <updates directory> -app-version <running version> -selfupdate-copy <temporary directory>`.
   It checks first that no other account can change the data directory,
   then reads only the updates directory, writes nothing there and prints
   one line on stdout; exit code 0 `ok version=X.Y.Z kind=K file=<relative
   path> sha256=<hex> copy=<absolute path>`: the package was copied, while
   it was hashed, into a new directory only this account may use
   (`<temporary directory>/lectureflow-update-<n>/<name>`; Windows: a
   protected DACL for the user, SYSTEM and Administrators; elsewhere 0700) —
   **run only that copy** (sec-update M3: nothing can swap the file between
   the check and the start), and remove it when done (an installer still
   runs from it while the app quits: a later start removes copies older
   than ten minutes); 1 `invalid <code>: <reason>` (do not install; delete
   `pending.json`); 2 `not_configured: …` (a build without a public key:
   nothing can have been verified, do not install); 3 `untrusted_dir: …`
   (other accounts can change the data directory: do not install, keep
   `pending.json` for a start after the permissions were tightened), or 3
   `untrusted_install_dir: …` (with `-selfupdate-install-dir`: other
   accounts can change the installation directory; the same, the user
   installs the new version by hand). Without
   `-selfupdate-copy` it writes nothing and the ok line has no `copy=`.
2. When previous.json has no `file`, back up the installation into
   `backup/<running version>/` (Windows portable: the executable; macOS: move
   the `.app`; Windows installed: the installation directory) and record it as
   `backup`. The desktop shell also records the SHA-256 of the whole tree in
   its own record (on Windows `trust.json` in
   `%LOCALAPPDATA%\LectureFlow\selfupdate`, see "The shell's record outside
   the data directory"; on macOS still the profile's
   `electron/selfupdate-trust.json`, which `CheckTrust` covers while the
   profile lies in the data directory). The hash is taken from
   the installation itself, before it is copied or moved, and the backup
   must have it once it lies in `backup/` (security audit M-3): a backup
   that differs is not recorded, and on Windows the update is not installed
   without one. Later only a backup that still has that hash is used. A
   backup of the running version that is already there serves again only
   while its recorded hash is the hash of the installation as it is now;
   any other backup or record of that version is removed before the update
   is installed (a backup and a record prepared together in a data
   directory that was once writable for others agree with each other, not
   with what is installed), and a new one is made where one is needed.
3. Record the installation (versions and the installation's place) in the
   shell's record (see above for where it lies), rename `pending.json` to `applying.json` (so a crash
   during the installation is noticed), then install:
   - `installer`: run it silently (NSIS `/S`), then start the installed app;
   - `portable`: replace the portable executable (`PORTABLE_EXECUTABLE_FILE`)
     with the new one;
   - `zip`: unpack the `.app` next to the current one, swap the two, keep the
     old one as the backup; `dmg`: mount, copy the `.app`, swap likewise.
4. The new version's Electron, finding `applying.json` with its own version,
   waits until its service is healthy, then deletes `applying.json` and writes
   result.json `installed`. When the new version does not become healthy
   (for example two starts in a row), restore from previous.json (`file`: run
   or copy that package; `backup`: put the backup back), delete
   `applying.json` and write `rolled_back`. A rollback runs nothing it has
   not checked (sec-update M2): `applying.json` counts only when the shell
   recorded that installation and its `target` is this installation's place;
   the package is checked like an update by the installed service program,
   `lectureflow -selfupdate-verify <updates directory> -selfupdate-previous
   -app-version <running version> -selfupdate-copy <temporary directory>`
   (previous.json's `file`: the signature of `<version>/update.json`, the
   version binding, older than the running version, size and SHA-256; the
   same exit codes; exit 1 `selfupdate.not_available` when previous.json
   names no package), and run from its copy. Exit 3 (or no answer at all):
   no rollback.
   A backup is never run or restored from where it lies (security audit
   M-3). The shell asks the service program for an empty directory only
   this account can use,
   `lectureflow -selfupdate-verify <updates directory> -app-version <running
   version> -selfupdate-private -selfupdate-copy <temporary directory>`
   (exit 0 `ok private=<temporary directory>/lectureflow-update-<n>`; 1
   `invalid …` when it cannot be made; 3 `untrusted_dir: …`; no public key
   is involved), copies the backup into `<that directory>/app` — after
   checking, without reading a file, that the backup holds no more files
   and bytes than were recorded — and hashes the copy: only a copy with the
   recorded hash is used, and only the copy. Windows portable: the copy
   replaces the executable; macOS: the copy is swapped in; Windows
   installed: a copy of the failed version itself (`<that
   directory>/helper`, the program that is running, started with
   `--lf-restore`) waits for the app to quit, checks the hash of `app`
   again and puts it into the installation directory. The shell's profile
   records that directory and the hash with the installation
   (`install.restore {dir, tree}`); the restore program does nothing from
   anywhere else. The directory is removed when done, or by a later start
   (copies older than ten minutes).

Go drops a `pending.json` whose version is not newer than the running version
(the update was installed), never reads or writes `applying.json`, and removes
at start: offline-update staging directories and version directories that are
neither the running version, the pending one, another copy's pending one
(below), previous.json's, nor the available one being downloaded (unless its
installation failed, see result.json). Other names (`backup/`) are left
alone. The one exception to both: "Restrict access" on a data directory that
was open to change removes the rollback state, `applying.json` and `backup/`
included (see the end of "Signature (minisign)").

### Copies sharing one data directory

The copy set up by the installer, the portable copy and an unpacked folder
of one account all use `%APPDATA%\LectureFlow` unless told otherwise, and
`pending.json`, `previous.json` and `applying.json` have one slot each. A
copy leaves alone what belongs to another one (r6; security review SEC29
section 6, case 6); it used to remove it and report a failure:

- **`applying.json` naming another place** (`target` is an absolute path
  that is not this installation's): the other copy's update on probation or
  rolling back. Electron leaves it, the backups, the database snapshot and
  previous.json exactly as they are and writes no result. (One naming this
  place without a matching record, no place or a relative one is dropped as
  before.)
- **`pending.json` of the other kind** (this platform, `kind` installer for
  the portable copy and the other way round), whatever its version:
  Electron neither installs, removes nor reports it. Go does not take it
  for its own (`Status.pending` stays empty, `DELETE
  /api/selfupdate/pending` leaves it), keeps its package directory, and
  starts no update of this copy meanwhile, because a download or an offline
  update ends by replacing previous.json and pending.json:
  `POST /api/selfupdate/download` and `POST /api/selfupdate/import` answer
  409 `selfupdate.other_copy_pending {version, kind}` before anything is
  fetched or staged (`kind` is a coded text, `{"code":
  "selfupdate.copy_installer"}` or `copy_portable`: the page puts that
  code's text, the name of the copy, into the sentence). A check still runs and the newer version is still
  shown as available; the refusal tells the user to start the other copy
  first. It ends when the other copy installs its update (its Electron
  renames pending.json; the file is read again at every attempt). Only a
  pending.json whose package is gone is removed, by either copy's Go.
  The other copy may be gone for good, so the wait has an end: **14 days**
  after `verified_at` (`OtherPendingMaxDays`) the download or offline
  update is no longer refused and takes the slot as it always did. The old
  pending.json is still not shown as this copy's and not removed for its
  age, by Go or by Electron. One whose `verified_at` lies more than a day
  ahead of this computer's clock (made while the clock was set far ahead)
  is given up as well, or it would hold this copy back until that day; a
  clock that is a little off is no reason. One without a readable
  `verified_at` is never given up.
- **A copy that cannot update itself** (an unpacked or copied folder):
  Electron leaves every readable `pending.json` as it is and writes no
  result (it used to remove it with "install the new version by hand").
  Electron tells the service: `-no-self-update` (`Options.NoInstall`). Go
  then takes no package for this copy's: a pending.json it finds is another
  copy's whatever its platform, kind or version (not shown, not removed,
  its package kept); a check still finds a newer version, shown without a
  package (`available.file` empty: the page says to download it from the
  releases page and install it by hand, and offers no download);
  `POST /api/selfupdate/download` and `/import` answer 409
  `selfupdate.no_package`. So such a copy never writes pending.json, and
  the by-hand hint comes before a download instead of after a restart.

Both sides decide by one rule whose a pending.json is (Go `otherCopys`,
Electron `otherCopysPending`; the same table is in both tests), and Go is
told the kind Electron decided: `-portable` for the portable build (the
variable `PORTABLE_EXECUTABLE_FILE` is dropped from the environment where
Electron does not accept it), `-no-self-update` for a copy that cannot
update itself, neither for the installer's installation and the Mac app.

Not covered (the slots stay single): two copies on probation at the same
time, and a copy that installs its own update while another copy's
`applying.json` lies there (it replaces it).

A rollback keeps the database it replaces: before the snapshot is put
back, `lectureflow.db`, `-wal` and `-shm` in the data directory are copied
to `lectureflow.db.lf-before-rollback` (`-wal` and `-shm` after that name),
one generation; a failure of this step is logged and the rollback goes on.
The database never leaves its name for it (a copy, not a rename): whatever
fails, or wherever the process dies, the data directory holds the database
or the snapshot. `applying.json` records the decision (`db_kept`) before
anything is replaced, so a rollback that starts over after a kill does not
keep again what by then may be the snapshot; and when the snapshot does
not get into the database's place at all, the copy is removed again.

Putting the snapshot back never leaves a database beside the log of
another one (reviews AUD2 L1, SEC40b PB-2: SQLite applies a stray `-wal`
to whatever database lies beside it). The steps (`_restoreDb` in
selfupdate.js), each retried like a rename while a file is in use:

1. copy the snapshot's files beside the database (`<name>.lf-restore`);
2. keep the database as it is now (above; after 1, so that on a nearly
   full disk the snapshot gets the room, not the kept copy);
3. rename the `-wal` and `-shm` that lie there aside (`<name>.lf-aside`);
4. the snapshot's database takes the place of `lectureflow.db`;
5. the snapshot's `-wal` / `-shm`, if it has them, follow;
6. remove what was moved aside.

| Step | When it fails | When the process is killed after it |
|---|---|---|
| 1 copy | database and log as they were → A | as they were, temporary files → R |
| 2 keep | as they were, nothing kept; goes on with 3 | as they were → R |
| 3 aside | moved back: as they were → A | the database without its last log; the log aside → R |
| 4 swap | log moved back: as they were → A | the snapshot's database, no log beside it → R |
| 5 follow | the snapshot's database without its log → B | the whole snapshot, stray aside files → R |
| 6 remove | the whole snapshot; a stray `.lf-aside` file, logged | the whole snapshot → R |
| undo of 3 / 4 fails too | the database without its last log (it lies beside it as `.lf-aside`) → B | |

- **A**: nothing of the snapshot is in place, this run's kept copy is
  removed. The rollback of the program goes on as it always did when the
  snapshot was not put back; the rollback's error in result.json now says
  that the database is as the new version left it, and why.
- **B** (two faults in a row): the database is a valid one that lacks its
  last log, never one with another's; the kept copy has it whole. The
  rollback stops, result.json says `failed` with the files' names, and
  `applying.json` stays, which makes the next start R.
- **R**: the next start of this installation runs the rollback again
  before it starts the service (`applying.json` still says `installing`
  with the tries used up). Every step can be done again from any of these
  states and ends with the whole snapshot.

In every state `lectureflow.db` exists and is a database of its own.

Nothing in LectureFlow opens the kept database. It holds the credentials
the database held: after a data directory move the one left in the old
directory is scrubbed like the old database (`relocate.ScrubOldDataDir`).

Known limitations (review AUD2 L2; low, left to a later round):

- **The kept database keeps its credentials.** They are the sealed values
  the database held at the rollback (the data key lies in the same data
  directory, protected by the account). A key the user removes or replaces
  afterwards is still in `lectureflow.db.lf-before-rollback` (and its
  `-wal`), until the next rollback replaces the kept copy, the data
  directory is moved (the old directory's copy is scrubbed) or the user
  deletes the files. The clean-up after a credential is replaced or removed
  (`internal/db` secure writes, `secrets.scrub_pending`,
  `check.credentials_scrub_pending`) does not touch it, on purpose: that
  path works on a database handle and knows no data directory; it also
  runs for writes nobody asked for (the Colab authorisation renewing
  itself, the sealing at a start, relocate's scrubs of other databases),
  so removing the files there would make the copy vanish at arbitrary
  times, and removing them at all gives up what the copy is for. The
  smaller and safer way is to scrub it like the databases imports replaced
  (`scrubImportBackupsOnce` at start: credentials removed, the rest stays
  readable), once per kept copy; not done yet.
- **The update's own snapshot** (`updates/backup/<version>/db`, made when
  an update is installed and removed when it is finished, so normally
  there only while the new version starts for the first time) holds the
  same credentials and is outside every clean-up: a move of the data
  directory during that time copies it and leaves the old directory's
  copy unscrubbed, and a rollback puts back a key that was removed
  during that time.

## HTTP API

| Request | Answer |
|---|---|
| `GET /api/selfupdate` | `Status` |
| `POST /api/selfupdate/check` | 200 `Status` after the check; a failed check is `error` (`selfupdate.signature_invalid`, `version_mismatch`, `manifest_invalid`, `no_manifest` (the release has no update.json or signature: HTTP 404), `unreachable`, the most telling of all sources; `detail` lists every source's answer, one line each) and `state: "error"` unless an update can still be installed (see below); 409 `not_configured`, `busy` |
| `POST /api/selfupdate/download` | 202 `Status` (`downloading`; for the version of `install_failed`: trying it again); 409 `not_configured`, `busy`, `not_available`, `too_old`, `no_package`, `untrusted_dir {path, who}`, `untrusted_install_dir {path, who}`, `other_copy_pending {version, kind}` |
| `DELETE /api/selfupdate/download` | 200 `Status`: the download is cancelled (the partial file stays) |
| `POST /api/selfupdate/import` | Desktop mode JSON `{"paths": [...]}` (or `{"path"}`), else multipart with one to three `file` parts. 200 `Status` (`pending`); 400 `signature_invalid`, `version_mismatch`, `manifest_invalid`, `checksum`, `import_incomplete {missing}`, `import_invalid {file}`; 409 `not_newer`, `too_old`, `no_package`, `not_configured`, `busy`, `untrusted_dir {path, who}`, `untrusted_install_dir {path, who}`, `other_copy_pending {version, kind}`; the upload and path codes of the other upload endpoints |
| `DELETE /api/selfupdate/pending` | 200 `Status`: the pending update is dropped, with its files (`pending.json` and `updates/<version>/`; the running version's files and `backup/` stay) |
| `GET /api/selfupdate/settings` | `{"auto_check": true, "sources": []}` |
| `PATCH /api/selfupdate/settings` | `{"auto_check"?, "sources"?}` → 200 settings; 400 `selfupdate.sources_invalid {index}` |

Every status change is announced as SSE `changed {kind: "selfupdate"}` (download
progress at most once a second).

`Status`:

```
state            not_configured | idle | checking | up_to_date | available |
                 downloading | importing | pending | error | install_failed
reason           not_configured: no_public_key | web_mode | dev_build |
                 unsupported_platform | no_source
current_version, channel (stable | beta), platform, kind, auto_check, last_check
next_check       the next automatic check (auto_check on, after a first check)
available        {version, channel, released_at, notes_url, min_version, direct,
                  file, size, source}   a newer version (newer than a pending one too)
progress         {bytes_done, bytes_total}       while downloading
pending          {version, file, size, origin, verified_at}
rollback         {version, package}   previous.json; package: a package or backup exists
last_result      result.json
install_failed   {version, at, outcome, error}   installing that newer version did
                 not work: not offered again until a newer one appears
error            {code, params, detail}          the last check or download failed
```

`available` and `pending` may both be present (a newer release appeared after
the download); the state is then `available`.

An update that can be installed comes first: a failed check leaves the
version found before `available` and a downloaded one `pending`, with the
failure in `error` (a note). `state: "error"` is a failed download of the
available version (`download_failed`, `checksum`, `io_failed`;
`IsDownloadError`), or a failed check when nothing can be installed. A
successful offline update clears the error.

`install_failed` is the state while that version is the newest one known
(`available` is then absent or that version, which may be downloaded again:
"try again") and nothing is pending; a failed check is a note (`error`).

## Releasing

See `scripts/release/README.md`: `make-update-json` (CI) builds the unsigned
update.json from the release files; `sign-update.ps1` / `sign-update.sh` sign it
on the maintainer's computer (by hand: `minisign -S -s lectureflow.key -m
update.json -x update.json.minisig -t "lectureflow update version:X.Y.Z"`;
minisign's default trusted comment is refused).

Without network a check fails with `selfupdate.unreachable`; while the latest
release carries no update.json or no signature (between publishing and
uploading the signature), with `selfupdate.no_manifest` ("the update
information is not ready yet", not a network problem). The settings show it,
the workbench does not, and the automatic checks back off as above (kept
across restarts; TestBuiltinKeyOfflineBacksOff).
