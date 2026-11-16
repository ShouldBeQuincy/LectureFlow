#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Builds the LGPL ffmpeg/ffprobe for macOS arm64 from the pinned source
# (packaging/third-party.json: url, sha256, configure). Run on macOS (CI:
# macos-15, cached). Output: desktop/.cache/ffmpeg-macos-<version>/
#   bin/{ffmpeg,ffprobe}
#   COPYING.LGPLv2.1, LICENSE.md, thirdparty/ (dav1d's COPYING and PATENTS)
#   BUILDINFO.txt   source, deployment target, compiler, SDK, configure line, otool -L
# The one external library, dav1d (AV1 software decoder, BSD-2-Clause), is
# pinned in deps (URL and SHA-256), built with meson as a static library and
# linked in. The build tools meson, ninja and pkgconf come from PyPI wheels
# pinned by SHA-256 (build_tools) into a private virtual environment; they
# are not shipped.
#
# Everything is compiled for the app's minimum macOS (third-party.json
# macos.minimum_system_version, exported as MACOSX_DEPLOYMENT_TARGET); the
# script checks the LC_BUILD_VERSION minos of ffmpeg, ffprobe and every
# object in libdav1d.a. It builds into <output>.partial and renames that only
# after every check passed (stage.js takes an existing bin/ffmpeg as a
# finished build), so a failed run never leaves a build that looks finished
# and keeps the previous one.
set -euo pipefail

cd "$(dirname "$0")/.."
manifest="$PWD/packaging/third-party.json"
read_json() { node -e "const m=require(process.argv[1]); console.log($1)" "$manifest"; }

[ "$(uname -s)" = Darwin ] || { echo "build-ffmpeg-macos.sh runs on macOS" >&2; exit 1; }
[ "$(uname -m)" = arm64 ] || { echo "build-ffmpeg-macos.sh builds on Apple silicon (arm64)" >&2; exit 1; }

version=$(read_json "m.ffmpeg.version")
url=$(read_json "m.ffmpeg['darwin-arm64'].url")
sha=$(read_json "m.ffmpeg['darwin-arm64'].sha256")
root=$(read_json "m.ffmpeg['darwin-arm64'].root")
# The app's minimum macOS (electron-builder minimumSystemVersion reads the same value).
target=$(read_json "m.macos.minimum_system_version")
export MACOSX_DEPLOYMENT_TARGET="$target"
prefix="$PWD/.cache/ffmpeg-macos-$version"
part="$prefix.partial"
work="$PWD/.cache/ffmpeg-src"
deps="$work/deps"
destdir="$work/destdir"
jobs=$(sysctl -n hw.ncpu)
configure_args=()
while IFS= read -r a; do a=${a/PREFIX/$prefix}; configure_args+=("${a//DEPS/$deps}"); done < <(read_json "m.ffmpeg['darwin-arm64'].configure.join('\n')")

for f in "${configure_args[@]}"; do
  case "$f" in --enable-gpl|--enable-nonfree|--enable-version3|--enable-libx264|--enable-libx265) echo "refusing $f (LGPL build only)" >&2; exit 1 ;; esac
done

fetch() { # url sha256 file
  local u=$1 s=$2 f=$3 got
  if [ -f "$f" ] && [ "$(shasum -a 256 "$f" | cut -d' ' -f1)" = "$s" ]; then return; fi
  curl -fsSL --retry 3 -o "$f.part" "$u"
  got=$(shasum -a 256 "$f.part" | cut -d' ' -f1)
  if [ "$got" != "$s" ]; then echo "SHA-256 mismatch for $u: $got, expected $s" >&2; rm -f "$f.part"; exit 1; fi
  mv "$f.part" "$f"
}

# minos FILE: the LC_BUILD_VERSION minos of a Mach-O file (the first one in an archive).
minos() { /usr/bin/otool -l "$1" | awk '/LC_BUILD_VERSION/{f=1} f && $1=="minos"{print $2; exit}'; }

rm -rf "${deps:?}" "${part:?}" "${destdir:?}"
mkdir -p "$work/downloads" "$deps" "$part/thirdparty"

# Build tools (meson, ninja, pkgconf): pinned wheels only (--require-hashes).
tools="$work/tools"
rm -rf "$tools"
# The pinned meson needs Python 3.10 or newer; the python3 of the Xcode
# command line tools is 3.9, so take the newest python3.x on PATH.
py=""
for c in python3.14 python3.13 python3.12 python3.11 python3.10 python3; do
  if command -v "$c" >/dev/null && "$c" -c 'import sys; sys.exit(sys.version_info < (3, 10))' 2>/dev/null; then py="$c"; break; fi
done
if [ -z "$py" ]; then echo "Python 3.10 or newer is needed for the build tools (e.g. brew install python@3.13)" >&2; exit 1; fi
"$py" -m venv "$tools"
read_json "m.ffmpeg['darwin-arm64'].build_tools.map(t => t.name + '==' + t.version + ' --hash=sha256:' + t.sha256).join('\n')" > "$work/build-tools.txt"
"$tools/bin/python" -m pip install --quiet --disable-pip-version-check --no-deps --only-binary :all: --require-hashes -r "$work/build-tools.txt"
export PATH="$tools/bin:$PATH"
if ! command -v pkg-config >/dev/null; then ln -s "$tools/bin/pkgconf" "$tools/bin/pkg-config"; fi
export PKG_CONFIG_LIBDIR="$deps/lib/pkgconfig"
export PKG_CONFIG_PATH=""

# dav1d: AV1 software decoder (FFmpeg's own av1 decoder needs hardware acceleration).
n=$(read_json "m.ffmpeg['darwin-arm64'].deps.length")
for ((i = 0; i < n; i++)); do
  name=$(read_json "m.ffmpeg['darwin-arm64'].deps[$i].name")
  [ "$name" = dav1d ] || { echo "unknown dependency $name" >&2; exit 1; }
  durl=$(read_json "m.ffmpeg['darwin-arm64'].deps[$i].url")
  droot=$(read_json "m.ffmpeg['darwin-arm64'].deps[$i].root")
  f="$work/downloads/$(basename "$durl")"
  fetch "$durl" "$(read_json "m.ffmpeg['darwin-arm64'].deps[$i].sha256")" "$f"
  rm -rf "${work:?}/$droot"
  tar -xf "$f" -C "$work"
  d="$work/$droot"
  echo "== dav1d"
  meson setup "$d/build" "$d" --prefix="$deps" --libdir=lib --buildtype=release --default-library=static \
    -Denable_asm=true -Denable_tools=false -Denable_examples=false -Denable_tests=false -Denable_docs=false
  ninja -C "$d/build" -j"$jobs" install
  # Every object of the static library is built for the deployment target.
  got=$(/usr/bin/otool -l "$deps/lib/libdav1d.a" | awk '$1=="minos"{print $2}' | sort -u | tr '\n' ' ')
  [ "$got" = "$target " ] || { echo "libdav1d.a objects have minos '$got', expected $target" >&2; exit 1; }
  cp "$d/COPYING" "$part/thirdparty/dav1d-COPYING.txt"
  # The AOMedia Patent License 1.0 (doc/PATENTS) asks binaries to carry it.
  cp "$d/doc/PATENTS" "$part/thirdparty/dav1d-PATENTS.txt"
done

tarball="$work/$(basename "$url")"
fetch "$url" "$sha" "$tarball"
rm -rf "${work:?}/$root"
tar -xf "$tarball" -C "$work"
cd "$work/$root"
./configure "${configure_args[@]}"
make -j"$jobs"
# LGPL only (SPEC-v3 V7), as in the Windows build.
grep -q '^#define FFMPEG_LICENSE "LGPL version 2.1 or later"$' config.h || { grep FFMPEG_LICENSE config.h >&2; echo "not an LGPL 2.1 build" >&2; exit 1; }
# Installed under DESTDIR, so the configured prefix (shown by -buildconf) is the final directory.
make install DESTDIR="$destdir"
mv "$destdir$prefix/bin" "$part/bin"
cp COPYING.LGPLv2.1 LICENSE.md "$part/"
for b in "$part/bin/ffmpeg" "$part/bin/ffprobe"; do
  # Only system libraries and frameworks may be linked dynamically.
  if /usr/bin/otool -L "$b" | tail -n +2 | awk '{print $1}' | grep -vE '^(/usr/lib/|/System/Library/)'; then
    echo "$(basename "$b") links a non-system library" >&2; exit 1
  fi
  m=$(minos "$b")
  [ "$m" = "$target" ] || { echo "$(basename "$b") minos $m, expected $target" >&2; exit 1; }
  /usr/bin/lipo -archs "$b" | grep -qx arm64 || { echo "$(basename "$b") is not arm64 only" >&2; exit 1; }
done
buildconf=$("$part/bin/ffmpeg" -hide_banner -buildconf)
echo "$buildconf"
# Not "ffmpeg | grep -q": grep stops at the first match, ffmpeg gets SIGPIPE
# and pipefail turns that into a failure.
decoders=$("$part/bin/ffmpeg" -hide_banner -decoders)
grep -q libdav1d <<<"$decoders" || { echo "libdav1d decoder missing" >&2; exit 1; }

{
  echo "FFmpeg $version for macOS arm64, built by desktop/scripts/build-ffmpeg-macos.sh"
  echo "source: $url"
  echo "source sha256: $sha"
  for ((i = 0; i < n; i++)); do
    echo "dependency: $(read_json "m.ffmpeg['darwin-arm64'].deps[$i].name + ' ' + m.ffmpeg['darwin-arm64'].deps[$i].version + ' ' + m.ffmpeg['darwin-arm64'].deps[$i].url + ' sha256 ' + m.ffmpeg['darwin-arm64'].deps[$i].sha256") (static library, meson)"
  done
  echo "deployment target: macOS $target (MACOSX_DEPLOYMENT_TARGET; minos of ffmpeg, ffprobe and libdav1d.a checked)"
  echo "compiler: $(clang --version | head -n 1)"
  echo "sdk: $(xcrun --show-sdk-version 2>/dev/null || echo unknown)"
  echo "build tools (PyPI wheels pinned by SHA-256, not shipped): $(read_json "m.ffmpeg['darwin-arm64'].build_tools.map(t => t.name + ' ' + t.version).join(', ')")"
  echo "license checks (SPEC-v3 V7): configure line without --enable-gpl, --enable-nonfree, --enable-version3; config.h FFMPEG_LICENSE \"LGPL version 2.1 or later\""
  echo "configure:"
  for a in "${configure_args[@]}"; do echo "  $a"; done
  echo "linked libraries (otool -L ffmpeg):"
  /usr/bin/otool -L "$part/bin/ffmpeg" | tail -n +2 | sed 's/^[[:space:]]*/  /'
} > "$part/BUILDINFO.txt"
rm -rf "${destdir:?}" "${prefix:?}"
mv "$part" "$prefix"
echo "ffmpeg $version for macOS $target: $prefix/bin"
