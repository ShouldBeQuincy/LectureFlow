# Release tools for the in-app update

The in-app update (SPEC-ext "应用内更新"; format and contract in
`internal/selfupdate/README.md`) needs two files on every GitHub release:
`update.json` (what the release contains: versions, package URLs, sizes,
SHA-256) and `update.json.minisig` (its minisign signature). CI makes the
first; the maintainer makes the second on their own computer, because only
that computer holds the password-protected secret key. Nobody else, CI
included, ever gets the key.

| File | Where it runs | What it does |
|---|---|---|
| `make-update-json/` | CI (release workflow) | `go run ./scripts/release/make-update-json -dir out -version 3.2.0 -repo OWNER/REPO`: finds the four packages in `out/`, hashes them, writes `out/update.json` (unsigned) and checks it with the application's own parser. |
| `make-update-json/` `-verify` | maintainer (the sign scripts run it) | Checks a downloaded `update.json` against the tag and the maintainer's own download of the packages: recomputes sizes and SHA-256, checks every URL, the version and the channel; any difference refuses signing (`verify.go`). |
| `sign-update.ps1` | maintainer, Windows | Runs that check, then signs `update.json` with minisign; the trusted comment is `lectureflow update version:<version>`. |
| `sign-update.sh` | maintainer, macOS / Linux | The same in bash. |

## The update key

| | |
|---|---|
| Public key (`PublicKey` in `internal/selfupdate/keys.go`) | `RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX` |
| Key id, as minisign prints it (`PublicKeyID`) | `0FAC4F1B6C17636D` |
| Made | 2026-10-27, with minisign, on the maintainer's computer |
| Secret key | a password-protected minisign key file (`lectureflow.key`) on the maintainer's computer only, plus an offline backup |

Every build from 3.2.0 on trusts this key and no other. The `.pub` file's
first line names the key id (`untrusted comment: minisign public key
0FAC4F1B6C17636D`), and so does `minisign -V` for a signature made with it.
`TestBuiltinPublicKey` checks that `PublicKey`, `PublicKeyID` and this page
agree. Nothing secret appears here or anywhere in the repository: the secret
key file and its password stay with the maintainer.

## Releasing a version

1. Push the version tag; the release workflow builds the packages and creates
   a **draft** release with them and the unsigned `update.json`.
2. On the maintainer's computer, with minisign 0.11 or later installed (from
   <https://github.com/jedisct1/minisign/releases>, or a package manager such
   as `winget install jedisct1.minisign` / `brew install minisign` /
   `scoop install minisign`) and a checkout of the tagged commit (the check
   below runs `go run` in it), download `update.json` **and the four
   packages** of the draft release, check and sign, upload the signature:

   ```
   gh release download v3.2.0 --repo ShouldBeQuincy/LectureFlow --dir release-v3.2.0 \
     --pattern update.json --pattern 'LectureFlow-Setup-3.2.0-x64.exe' --pattern 'LectureFlow-3.2.0-x64-portable.exe' \
     --pattern 'LectureFlow-3.2.0-arm64.dmg' --pattern 'LectureFlow-3.2.0-arm64*.zip'
   scripts/release/sign-update.sh -t v3.2.0 -d release-v3.2.0 -m release-v3.2.0/update.json \
     -s ~/keys/lectureflow.key -p ~/keys/lectureflow.pub
   # Windows: powershell -NoProfile -ExecutionPolicy Bypass -File scripts\release\sign-update.ps1 `
   #   -Tag v3.2.0 -PackageDir release-v3.2.0 -Manifest release-v3.2.0\update.json `
   #   -SecretKey D:\keys\lectureflow.key -PublicKey D:\keys\lectureflow.pub
   gh release upload v3.2.0 release-v3.2.0/update.json.minisig --repo ShouldBeQuincy/LectureFlow
   ```

   Before minisign or the key is touched, the scripts run
   `go run ./scripts/release/make-update-json -verify <update.json> -dir <packages> -tag <tag>`.
   It reads update.json with the application's own parser and refuses to
   sign, naming the item, when
   - a package's size or SHA-256 differs from the file in the package
     directory (an installer or update.json changed after CI made it);
   - a URL is not `https://github.com/ShouldBeQuincy/LectureFlow/releases/download/<tag>/<name>`
     (the repository is `Repo` in `internal/selfupdate/keys.go`), or a
     package's name is not the one `make-update-json` gives it;
   - the version is not the tag's, or the channel is not `beta` for a
     pre-release version and `stable` otherwise (`-c` / `-Channel` signs
     another channel on purpose);
   - update.json lists a package the directory lacks, an unknown platform or
     kind, or leaves out one of the four packages;
   - `notes_url` is not on `https://github.com/ShouldBeQuincy/LectureFlow/`;
   - update.json has mirror rules (CI makes none). To sign them on purpose,
     pass `-A` / `-AllowMirrors`; they are then printed with the packages.

   It prints every package with its size and SHA-256 and the SHA-256 of the
   update.json it checked. Read that list before typing the password.
   minisign reads update.json again after the password prompt, so the
   scripts compare the file's SHA-256 with the checked one right before
   minisign and after it, and delete the new signature if it changed.
   The check runs `go run` with `GOTOOLCHAIN=local GOPROXY=off
   GOFLAGS=-mod=readonly`: it never downloads a module or a Go toolchain
   (with an incomplete module cache it fails; run `go mod download` once,
   separately). The scripts download nothing else either: fetch the files
   with `gh` (or the release page) as above.

   **What the check does not do.** The packages it compares with come from
   the same draft release as update.json. If CI is compromised, or someone
   replaces the packages **and** update.json together consistently, the
   check still passes: it catches an update.json that disagrees with the
   packages or points elsewhere, not a bad build. Trust in what CI built
   still rests on the workflow and its inputs. A cheap extra look before
   signing: the Windows installer's signer
   (`Get-AuthenticodeSignature .\release-v3.2.0\LectureFlow-Setup-3.2.0-x64.exe`)
   and the macOS app's notarization (`spctl -a -vv` on the app from the dmg),
   when those builds are signed.

   minisign asks for the key's password; the scripts never read, print or
   store the password or the key. Right after signing they verify the new
   signature against update.json on disk (`minisign -V`, which prints
   `Signature and comment signature verified`), with the key in `-p` /
   `-PublicKey` or, without it, the key the application is built with
   (`PublicKey` in `internal/selfupdate/keys.go`, read from there). This
   also catches update.json being swapped and restored while minisign read
   it. Any failure after minisign ran deletes the new `update.json.minisig`,
   and a `.minisig` left by an earlier run is deleted before the check, so
   a failed run never leaves a signature behind.
3. Publish the release as a normal release: GitHub's
   `releases/latest/download/update.json` (what the stable channel asks)
   never points at a draft or a pre-release; a pre-release is found only by
   the beta channel (releases API). Installed copies find it at their next
   daily check.

Do not edit `update.json` after signing: any change, even one byte, makes
every installation refuse it. To change it, sign it again.

### Signing by hand

Run the check first, from the repository root (it needs no key):

```
GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=readonly go run ./scripts/release/make-update-json -verify release-v3.2.0/update.json -dir release-v3.2.0 -tag v3.2.0
```

and sign only a file whose SHA-256 is the `checked update.json sha256` it
printed (check again after signing).

Once it prints `verified: 4 packages …`, the scripts run exactly this
(`3.2.0` = the tag without `v`, which the check made sure is update.json's
`version`):

```
minisign -S -s lectureflow.key -m update.json -x update.json.minisig -t "lectureflow update version:3.2.0" -c "LectureFlow 3.2.0 update.json"
```

- `-s` the secret key file, `-m` the file to sign, `-x` the signature file
  (next to update.json, uploaded as `update.json.minisig`), `-c` the
  untrusted comment (free text, not checked).
- `-t` the **trusted comment**, signed together with the signature:
  `lectureflow update version:<version>`. The version must equal update.json's
  `version`; more `key:value` fields may follow and are ignored
  (`lectureflow update version:3.2.0 channel:stable`). The short form
  `minisign -Sm update.json -s lectureflow.key` signs minisign's default
  trusted comment (`timestamp:… file:update.json …`), which every
  installation refuses (`selfupdate.version_mismatch`): always pass `-t`, or
  use the scripts, which also check the result.
- minisign 0.11 and later make prehashed signatures (`ED`); legacy ones
  (`-l`, `Ed`) are accepted as well. No other option is needed.

Check a signature (also after downloading it from the release) against the
built-in key, without any file:

```
minisign -V -P RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX -m update.json -x update.json.minisig
```

It must print `Signature and comment signature verified` and
`Trusted comment: lectureflow update version:3.2.0`.

## Recommended: release secrets in a protected Environment

The release workflow reads the code-signing and notarization secrets
(`WIN_CSC_LINK`, `WIN_CSC_KEY_PASSWORD`, `MAC_CSC_LINK`,
`MAC_CSC_KEY_PASSWORD`, `APPLE_API_KEY_P8`, `APPLE_API_KEY_ID`,
`APPLE_API_ISSUER`) as **repository** secrets: any workflow that someone
with write access pushes can read them. The update key is not among them
(it never leaves the maintainer's computer). Recommended, done by the
repository owner on GitHub, in this order:

1. Settings → Environments → New environment, e.g. `release`.
2. Deployment branches and tags: "Selected branches and tags", with the tag
   rule `v*` only.
3. Required reviewers: the maintainer. A release build then waits for an
   approval in the Actions tab (with a single maintainer, leave "Prevent
   self-review" off).
4. Move the seven secrets above from the repository secrets into this
   environment, then delete the repository copies.
5. Only then add `environment: release` to the `build` job of
   `.github/workflows/release.yml`, the job that uses them. A workflow that
   names the environment before it exists with the secrets builds unsigned
   installers (GitHub creates an empty environment).

The Actions caches the release build restores (ffmpeg, and aria2c on
macOS, shared with `ci.yml`) are a separate matter: `desktop/scripts/stage.js`
reuses a cached build only when its `BUILDINFO.txt` names the pinned version
and sources (otherwise it deletes it and builds again), and records the
SHA-256 of the binaries it ships in `build/stage-info.json`. The repository
pins the sources, not the built binaries, so a tampered cache that keeps
`BUILDINFO.txt` intact is not caught there.

## Making a key pair (done on 2026-10-27)

```
minisign -G -p lectureflow.pub -s lectureflow.key
```

Choose a strong password; keep `lectureflow.key` and its password only on the
maintainer's computer (and an offline backup). The base64 line of
`lectureflow.pub` (the second line, `RW…`) is `PublicKey` in
`internal/selfupdate/keys.go`, its key id `PublicKeyID`; `Repo` names the
repository. A build without a key (`PublicKey` empty, or built with
`-ldflags "-X lectureflow/internal/selfupdate.builtinPublicKey="`, as the
desktop end-to-end tests of the electron-updater path do) reports updates as
"not configured" and makes no request.

## Changing the key

An installation trusts only the key it was built with, and there is no way
to withdraw it from installed copies. So a new key reaches users through one
**bridge release** signed with the old key:

1. Make the new pair (`minisign -G -p lectureflow-2.pub -s lectureflow-2.key`,
   a new password). Its key id is in the first line of the `.pub` file.
2. Put the new base64 line into `PublicKey` and its key id into
   `PublicKeyID`, update the table above and the key paragraph of
   `internal/selfupdate/README.md` (`TestBuiltinPublicKey` fails until
   they agree), and release that version.
3. Sign this bridge release's update.json with the **old** key: installed
   copies verify it with the key they have, download it, and their own
   service program checks it again before it is installed. Keep it the latest
   stable release for a while.
4. Sign every later release with the new key only. A copy that missed the
   bridge sees the newer release, refuses its signature
   (`selfupdate.signature_invalid`, "signed with key …, not with the built-in
   key …") and has to install the bridge, or the new version, by hand once.

The bridge version cannot roll back to a package of the old key (its
`-selfupdate-previous` check uses the new key); only the shell's backup of the
installation brings back the previous version, so test the bridge release
with particular care. When the old secret key is **lost**, no bridge can be
signed: release the new key in a normal version and tell users to install it
by hand. When it may have been **stolen**, do the same at once and say so in
the release notes: whoever holds it can sign updates that installed copies
accept, until they have the version with the new key.

To check this package against the real minisign:
`go test -tags minisign -run RealMinisign ./internal/selfupdate/` (it makes a
throwaway key in a temporary directory).

## Release workflow steps (in `.github/workflows/release.yml`)

The workflow already has these steps. In the `draft-release` job, check out
the repository and set up Go **before**
downloading the installers (checkout cleans the workspace), then make
`update.json` before `gh release create` (which uploads `out/*`, so
`update.json` goes up with the packages):

```yaml
  draft-release:
    needs: build
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8.0.2
        with:
          pattern: installers-*
          path: out
          merge-multiple: true
      - name: update.json (unsigned; the maintainer signs it, see scripts/release/README.md)
        run: |
          go run ./scripts/release/make-update-json -dir out \
            -version "${GITHUB_REF_NAME#v}" -tag "${GITHUB_REF_NAME}" -repo "${GITHUB_REPOSITORY}"
      - name: Create the draft release
        # unchanged
```

The build jobs already copy `*.exe`, `*.dmg` and `*.zip` into `out/`; the macOS
zip keeps electron-builder's default name (`LectureFlow-<version>-arm64-mac.zip`),
which `make-update-json` expects (it also accepts `…-arm64.zip`). Add
`-min-version X.Y.Z` when a release cannot be reached from older versions
directly, and `-mirror PREFIX=REPLACE` for mirror rules listed in update.json.
