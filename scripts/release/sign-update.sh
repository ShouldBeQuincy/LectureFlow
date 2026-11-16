#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Signs a release's update.json with minisign, on the maintainer's computer
# only (SPEC-ext "应用内更新"; internal/selfupdate/README.md). CI never holds
# the secret key.
#
#   scripts/release/sign-update.sh -t v3.2.0 -d PACKAGE_DIR -s ~/keys/lectureflow.key
#       [-m update.json] [-c stable|beta] [-A] [-p lectureflow.pub] [-M minisign]
#
# Before the key is touched, update.json is checked against the tag being
# signed and against the packages in PACKAGE_DIR (the maintainer's own
# download of the release's four packages): make-update-json -verify reads it
# with the application's parser, recomputes every package's size and SHA-256,
# checks that each URL is https://github.com/<repo>/releases/download/<tag>/<name>
# and that version and channel are the tag's (-c sets the channel; default:
# beta for a pre-release version, else stable), and that update.json lists
# exactly the packages in PACKAGE_DIR. Mirror rules are refused unless -A.
# Any difference refuses signing and names the item. The check runs
# "go run" in this repository with GOTOOLCHAIN=local GOPROXY=off
# GOFLAGS=-mod=readonly, so it never downloads a module or a toolchain (it
# fails instead when the module cache lacks one).
#
# minisign reads update.json again after the password prompt, so the script
# compares the file's SHA-256 with the bytes the check read, right before
# minisign and after it; a change deletes the new signature and fails.
#
# minisign asks for the key's password itself; this script never reads,
# prints or stores the password or the key. The trusted comment is
# "lectureflow update version:<version of the tag>", which the application
# checks. The new signature is then verified against update.json with the
# public key the application is built with (PublicKey in
# internal/selfupdate/keys.go), or with -p PUBLIC_KEY_FILE; every failure
# after minisign ran deletes the new signature. A signature left by an
# earlier run is deleted first, so a failed run leaves none.
# Writes <update.json>.minisig next to update.json; upload that file to the
# release (see scripts/release/README.md).
set -euo pipefail

secret="" manifest="update.json" pubkey="" minisign="minisign" tag="" pkgdir="" channel="" allow_mirrors=""
usage() { echo "usage: $0 -t TAG -d PACKAGE_DIR -s SECRET_KEY [-m update.json] [-c stable|beta] [-A] [-p PUBLIC_KEY] [-M minisign]" >&2; exit 2; }
fail() { echo "sign-update: $*" >&2; exit 1; }
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | sed 's/^\\//' | cut -d' ' -f1
}
while getopts "s:m:p:M:t:d:c:Ah" opt; do
  case "$opt" in
    A) allow_mirrors=1 ;;
    s) secret="$OPTARG" ;;
    m) manifest="$OPTARG" ;;
    p) pubkey="$OPTARG" ;;
    M) minisign="$OPTARG" ;;
    t) tag="$OPTARG" ;;
    d) pkgdir="$OPTARG" ;;
    c) channel="$OPTARG" ;;
    *) usage ;;
  esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] && [ -n "$secret" ] && [ -n "$tag" ] && [ -n "$pkgdir" ] || usage

[ -f "$manifest" ] || fail "no such file: $manifest"
sig="$manifest.minisig"
if [ -e "$sig" ] || [ -L "$sig" ]; then
  rm -f "$sig" 2>/dev/null || true
  [ ! -e "$sig" ] && [ ! -L "$sig" ] || fail "cannot delete the old signature $sig"
  echo "Deleted the old signature $sig."
fi
[ -d "$pkgdir" ] || fail "no such directory: $pkgdir (download the release's packages into it first)"
command -v go >/dev/null 2>&1 || fail "go not found: the check before signing runs make-update-json -verify"

# 1. update.json against the tag and the local packages; no key is needed yet.
root="$(cd "$(dirname "$0")/../.." && pwd)"
manifest_abs="$(cd "$(dirname "$manifest")" && pwd)/$(basename "$manifest")"
pkgdir_abs="$(cd "$pkgdir" && pwd)"
verify=(go run ./scripts/release/make-update-json -verify "$manifest_abs" -dir "$pkgdir_abs" -tag "$tag")
[ -z "$channel" ] || verify+=(-channel "$channel")
[ -z "$allow_mirrors" ] || verify+=(-allow-mirrors)
if ! out="$(cd "$root" && GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=readonly "${verify[@]}")"; then
  [ -z "$out" ] || printf '%s\n' "$out"
  fail "update.json does not match tag $tag and the packages in $pkgdir; nothing was signed"
fi
printf '%s\n' "$out"
checked="$(printf '%s\n' "$out" | sed -n 's/^checked update.json sha256 \([0-9a-f]\{64\}\)$/\1/p')"
[ -n "$checked" ] || fail "the check printed no SHA-256 of update.json; nothing was signed"

# 2. Sign what was checked.
[ -f "$secret" ] || fail "the secret key file does not exist"
[ -z "$pubkey" ] || [ -f "$pubkey" ] || fail "no such file: $pubkey"
command -v "$minisign" >/dev/null 2>&1 || fail "minisign not found (install minisign 0.11 or later, or pass -M PATH)"
builtin_key=""
if [ -z "$pubkey" ]; then
  # The key the application is built with; not stored a second time.
  builtin_key="$(sed -n 's/^const PublicKey = "\(RW[A-Za-z0-9+\/=]*\)"[[:space:]]*$/\1/p' "$root/internal/selfupdate/keys.go" | tr -d '\r')"
  [ -n "$builtin_key" ] || fail "no built-in public key in internal/selfupdate/keys.go; pass -p PUBLIC_KEY"
fi

version="${tag#v}"
trusted="lectureflow update version:$version"
# After minisign ran, every failure deletes what it wrote.
fail_signed() { rm -f "$sig"; fail "$* (the signature was deleted)"; }
[ "$(sha256_of "$manifest")" = "$checked" ] || fail "$manifest changed after the check; nothing was signed"
echo "Signing $manifest (LectureFlow $version); minisign asks for the key's password."
"$minisign" -S -s "$secret" -m "$manifest" -x "$sig" -t "$trusted" -c "LectureFlow $version update.json" \
  || fail_signed "minisign failed; nothing was signed"
[ "$(sha256_of "$manifest")" = "$checked" ] || fail_signed "$manifest changed while minisign signed it"
[ "$(sed -n 3p "$sig" | tr -d '\r')" = "trusted comment: $trusted" ] || fail_signed "$sig does not carry the trusted comment '$trusted'"
if [ -n "$pubkey" ]; then
  "$minisign" -V -p "$pubkey" -m "$manifest" -x "$sig" || fail_signed "the new signature does not verify with $pubkey"
else
  "$minisign" -V -P "$builtin_key" -m "$manifest" -x "$sig" \
    || fail_signed "the new signature does not verify against $manifest with the built-in key $builtin_key"
fi
echo "Wrote $sig. Upload it to the draft release, e.g.:"
echo "  gh release upload $tag $sig --repo OWNER/REPO"
