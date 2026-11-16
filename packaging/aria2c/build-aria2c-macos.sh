#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Builds aria2c for macOS arm64 from the pinned aria2 release tarball
# (desktop/packaging/third-party.json: aria2c.darwin-arm64 url, sha256,
# configure; macos.minimum_system_version, the app's minimum macOS, as the
# deployment target). aria2 publishes no macOS binary of 1.37.0.
# Run on macOS (CI: macos-15). Output:
#   desktop/.cache/aria2c-macos-<version>/
#     bin/aria2c                 the program (ad-hoc signed)
#     COPYING, LICENSE.OpenSSL, AUTHORS
#     thirdparty/wslay-COPYING.txt   (WebSocket library inside the aria2 source)
#     BUILDINFO.txt              source, configure line, compiler, SDK, aria2c --version
#
# TLS is Apple's (AppleTLS: Security.framework, the system keychain's
# trust store). OpenSSL, GnuTLS, nettle, GMP, libgcrypt, libxml2, expat,
# SQLite, libssh2, c-ares, BitTorrent and Metalink are off: LectureFlow only
# downloads over HTTP(S) through the JSON-RPC interface. The only libraries
# linked are macOS system libraries (libz, libc++, libSystem, libiconv and
# the CoreFoundation / Security frameworks); the script fails otherwise
# (otool -L), when aria2c --version names a disabled library or feature, or
# when the minimum macOS version (LC_BUILD_VERSION minos) differs from
# macos.minimum_system_version.
#
# The build runs with PATH limited to the system directories and without
# CPPFLAGS / LDFLAGS / pkg-config from the environment, so nothing from
# Homebrew or MacPorts can be picked up. Corresponding source (GPL): the
# pinned tarball plus this script and the configure line in BUILDINFO.txt.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
manifest="$repo/desktop/packaging/third-party.json"
read_json() { node -e "const m=require(process.argv[1]); const v=($1); console.log(Array.isArray(v) ? v.join('\n') : v)" "$manifest"; }

version=$(read_json "m.aria2c.version")
url=$(read_json "m.aria2c['darwin-arm64'].url")
sha=$(read_json "m.aria2c['darwin-arm64'].sha256")
root=$(read_json "m.aria2c['darwin-arm64'].root")
# The app's minimum macOS (electron-builder minimumSystemVersion reads the same value).
target=$(read_json "m.macos.minimum_system_version")
forbidden=$(read_json "m.aria2c['darwin-arm64'].forbidden_configure")
cache="$repo/desktop/.cache"
out="$cache/aria2c-macos-$version"
# Built into $part and renamed to $out only after every check passed:
# stage.js takes an existing $out/bin/aria2c as a finished build.
part="$out.partial"
work="$cache/aria2c-src"
jobs=$(sysctl -n hw.ncpu)

configure_args=()
while IFS= read -r a; do configure_args+=("$a"); done < <(read_json "m.aria2c['darwin-arm64'].configure")
for f in "${configure_args[@]}"; do
  while IFS= read -r bad; do
    [ "$f" = "$bad" ] && { echo "refusing $f (third-party.json forbidden_configure)" >&2; exit 1; }
  done <<<"$forbidden"
done

[ "$(uname -s)" = Darwin ] || { echo "build-aria2c-macos.sh runs on macOS" >&2; exit 1; }
[ "$(uname -m)" = arm64 ] || { echo "build-aria2c-macos.sh builds on Apple silicon (arm64)" >&2; exit 1; }

fetch() { # url sha256 file
  local u=$1 s=$2 f=$3 got
  if [ -f "$f" ] && [ "$(shasum -a 256 "$f" | cut -d' ' -f1)" = "$s" ]; then return; fi
  curl -fsSL --retry 3 -o "$f.part" "$u"
  got=$(shasum -a 256 "$f.part" | cut -d' ' -f1)
  if [ "$got" != "$s" ]; then echo "SHA-256 mismatch for $u: $got, expected $s" >&2; rm -f "$f.part"; exit 1; fi
  mv "$f.part" "$f"
}

mkdir -p "$work/downloads"
tarball="$work/downloads/$(basename "$url")"
fetch "$url" "$sha" "$tarball"
rm -rf "${work:?}/$root" "${out:?}" "${part:?}"
tar -xf "$tarball" -C "$work"
src="$work/$root"

# A clean environment: system tools only, no flags or pkg-config paths
# from the caller (Homebrew's libraries must not leak into the build).
clean_env=(env -i "HOME=$HOME" "PATH=/usr/bin:/bin:/usr/sbin:/sbin" "LANG=C" "TMPDIR=${TMPDIR:-/tmp}"
  "MACOSX_DEPLOYMENT_TARGET=$target" "PKG_CONFIG=/usr/bin/false"
  "CC=clang" "CXX=clang++"
  "CFLAGS=-O2 -arch arm64 -mmacosx-version-min=$target -ffile-prefix-map=$work=/build"
  "CXXFLAGS=-O2 -arch arm64 -mmacosx-version-min=$target -ffile-prefix-map=$work=/build"
  "LDFLAGS=-arch arm64 -mmacosx-version-min=$target -Wl,-dead_strip")

cd "$src"
"${clean_env[@]}" ./configure "${configure_args[@]}" | tee "$work/configure.out"
grep -q '^AppleTLS:       yes' "$work/configure.out" || { echo "configure did not enable AppleTLS" >&2; exit 1; }
for lib in OpenSSL GnuTLS LibNettle LibGmp LibGcrypt LibXML2 LibExpat SQLite3 LibCares Libssh2 Bittorrent Metalink; do
  if grep -E "^$lib: +yes" "$work/configure.out" >/dev/null; then echo "configure enabled $lib" >&2; exit 1; fi
done
"${clean_env[@]}" make -j"$jobs"

mkdir -p "$part/bin" "$part/thirdparty"
cp src/aria2c "$part/bin/aria2c"
# strip invalidates the linker's ad-hoc signature; Apple silicon runs
# only signed code, so sign it again (ad hoc; electron-builder signs the
# whole app with the real identity when there is one).
/usr/bin/strip -x "$part/bin/aria2c"
/usr/bin/codesign --force --sign - "$part/bin/aria2c"
/usr/bin/codesign --verify "$part/bin/aria2c"
cp COPYING LICENSE.OpenSSL AUTHORS "$part/"
cp deps/wslay/COPYING "$part/thirdparty/wslay-COPYING.txt"

# Only system libraries and frameworks may be linked dynamically.
if /usr/bin/otool -L "$part/bin/aria2c" | tail -n +2 | awk '{print $1}' | grep -vE '^(/usr/lib/|/System/Library/)'; then
  echo "aria2c links a non-system library" >&2; exit 1
fi
minos=$(/usr/bin/otool -l "$part/bin/aria2c" | awk '/LC_BUILD_VERSION/{f=1} f && $1=="minos"{print $2; exit}')
[ "$minos" = "$target" ] || { echo "aria2c minos $minos, expected $target" >&2; exit 1; }
/usr/bin/lipo -archs "$part/bin/aria2c" | grep -qx arm64 || { echo "aria2c is not arm64 only" >&2; exit 1; }

ver=$("$part/bin/aria2c" --version)
head -n 1 <<<"$ver" | grep -qx "aria2 version $version" || { echo "unexpected version: $(head -n 1 <<<"$ver")" >&2; exit 1; }
libs=$(grep '^Libraries:' <<<"$ver" || true)
features=$(grep '^Enabled Features:' <<<"$ver" || true)
grep -q 'AppleTLS' <<<"$libs" || { echo "AppleTLS missing: $libs" >&2; exit 1; }
grep -q 'HTTPS' <<<"$features" || { echo "HTTPS missing: $features" >&2; exit 1; }
if grep -Eiq 'OpenSSL|GnuTLS|nettle|GMP|libgcrypt|libxml2|expat|sqlite3|libssh2|c-ares' <<<"$libs"; then echo "a disabled library is linked: $libs" >&2; exit 1; fi
if grep -Eq 'BitTorrent|Metalink|SFTP|Firefox3 Cookie' <<<"$features"; then echo "a disabled feature is enabled: $features" >&2; exit 1; fi

{
  echo "aria2 $version for macOS arm64, built by packaging/aria2c/build-aria2c-macos.sh"
  echo "source: $url"
  echo "source sha256: $sha"
  echo "deployment target: macOS $target"
  echo "compiler: $(clang --version | head -n 1)"
  echo "sdk: $(xcrun --show-sdk-version 2>/dev/null || echo unknown)"
  echo "environment: PATH=/usr/bin:/bin:/usr/sbin:/sbin, CC=clang, CXX=clang++,"
  echo "  CFLAGS/CXXFLAGS=-O2 -arch arm64 -mmacosx-version-min=$target -ffile-prefix-map=<build dir>=/build,"
  echo "  LDFLAGS=-arch arm64 -mmacosx-version-min=$target -Wl,-dead_strip; then strip -x and an ad-hoc signature"
  echo "configure:"
  for a in "${configure_args[@]}"; do echo "  $a"; done
  echo "linked libraries (otool -L):"
  /usr/bin/otool -L "$part/bin/aria2c" | tail -n +2 | sed 's/^[[:space:]]*/  /'
  echo "aria2c --version:"
  sed 's/^/  /' <<<"$ver"
} > "$part/BUILDINFO.txt"
mv "$part" "$out"
echo "aria2c $version: $out/bin/aria2c ($(shasum -a 256 "$out/bin/aria2c" | cut -d' ' -f1))"
