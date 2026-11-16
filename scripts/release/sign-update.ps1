# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
#
# Signs a release's update.json with minisign, on the maintainer's computer
# only (SPEC-ext "应用内更新"; internal/selfupdate/README.md). CI never holds
# the secret key.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\release\sign-update.ps1 `
#       -Tag v3.2.0 -PackageDir .\release-v3.2.0 -SecretKey D:\keys\lectureflow.key `
#       [-Manifest .\update.json] [-Channel stable|beta] [-AllowMirrors] [-PublicKey .\lectureflow.pub] [-Minisign minisign]
#
# Before the key is touched, update.json is checked against the tag being
# signed and against the packages in -PackageDir (the maintainer's own
# download of the release's four packages): make-update-json -verify reads it
# with the application's parser, recomputes every package's size and SHA-256,
# checks that each URL is https://github.com/<repo>/releases/download/<tag>/<name>
# and that version and channel are the tag's (-Channel sets the channel;
# default: beta for a pre-release version, else stable), and that update.json
# lists exactly the packages in -PackageDir. Mirror rules are refused unless
# -AllowMirrors. Any difference refuses signing and names the item. The
# check runs "go run" in this repository with GOTOOLCHAIN=local GOPROXY=off
# GOFLAGS=-mod=readonly, so it never downloads a module or a toolchain (it
# fails instead when the module cache lacks one). The same check as
# sign-update.sh.
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
# internal/selfupdate/keys.go), or with -PublicKey FILE; every failure after
# minisign ran deletes the new signature. A signature left by an earlier run
# is deleted first, so a failed run leaves none.
# Writes <Manifest>.minisig next to update.json; upload that file to the
# release (see scripts/release/README.md).

param(
  [Parameter(Mandatory = $true)][string]$Tag,
  [Parameter(Mandatory = $true)][string]$PackageDir,
  [Parameter(Mandatory = $true)][string]$SecretKey,
  [string]$Manifest = "update.json",
  [string]$Channel = "",
  [switch]$AllowMirrors,
  [string]$PublicKey = "",
  [string]$Minisign = "minisign"
)

$ErrorActionPreference = "Stop"

function Fail([string]$msg) {
  [Console]::Error.WriteLine("sign-update: $msg")
  exit 1
}

if (-not (Test-Path -LiteralPath $Manifest -PathType Leaf)) { Fail "no such file: $Manifest" }
$sig = "$Manifest.minisig"
if (Test-Path -LiteralPath $sig) {
  try { Remove-Item -LiteralPath $sig -Force } catch { }
  if (Test-Path -LiteralPath $sig) { Fail "cannot delete the old signature $sig" }
  Write-Host "Deleted the old signature $sig."
}
if (-not (Test-Path -LiteralPath $PackageDir -PathType Container)) {
  Fail "no such directory: $PackageDir (download the release's packages into it first)"
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Fail "go not found: the check before signing runs make-update-json -verify" }

# 1. update.json against the tag and the local packages; no key is needed yet.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..\..")).Path
$manifestAbs = (Resolve-Path -LiteralPath $Manifest).Path
$packageDirAbs = (Resolve-Path -LiteralPath $PackageDir).Path
$verify = @("run", "./scripts/release/make-update-json", "-verify", $manifestAbs, "-dir", $packageDirAbs, "-tag", $Tag)
if ($Channel) { $verify += @("-channel", $Channel) }
if ($AllowMirrors) { $verify += @("-allow-mirrors") }
function Sha256Of([string]$path) { (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() }
$goEnv = @{ GOTOOLCHAIN = "local"; GOPROXY = "off"; GOFLAGS = "-mod=readonly" }
$savedEnv = @{}
foreach ($k in $goEnv.Keys) {
  $savedEnv[$k] = [Environment]::GetEnvironmentVariable($k, "Process")
  [Environment]::SetEnvironmentVariable($k, $goEnv[$k], "Process")
}
Push-Location -LiteralPath $root
try {
  $out = @(& go @verify)
  $verifyExit = $LASTEXITCODE
} finally {
  Pop-Location
  foreach ($k in $goEnv.Keys) { [Environment]::SetEnvironmentVariable($k, $savedEnv[$k], "Process") }
}
$out | ForEach-Object { Write-Host $_ }
if ($verifyExit -ne 0) { Fail "update.json does not match tag $Tag and the packages in $PackageDir; nothing was signed" }
$checked = ""
foreach ($line in $out) {
  if ("$line" -match '^checked update\.json sha256 ([0-9a-f]{64})$') { $checked = $Matches[1] }
}
if (-not $checked) { Fail "the check printed no SHA-256 of update.json; nothing was signed" }

# 2. Sign what was checked.
if (-not (Test-Path -LiteralPath $SecretKey -PathType Leaf)) { Fail "the secret key file does not exist" }
if ($PublicKey -and -not (Test-Path -LiteralPath $PublicKey -PathType Leaf)) { Fail "no such file: $PublicKey" }
if (-not (Get-Command $Minisign -ErrorAction SilentlyContinue)) {
  Fail "minisign not found (install minisign 0.11 or later, or pass -Minisign PATH)"
}

$builtinKey = ""
if (-not $PublicKey) {
  # The key the application is built with; not stored a second time.
  $keysGo = Join-Path $root "internal\selfupdate\keys.go"
  foreach ($line in [System.IO.File]::ReadAllLines($keysGo)) {
    if ($line -match '^const PublicKey = "(RW[A-Za-z0-9+/=]*)"\s*$') { $builtinKey = $Matches[1] }
  }
  if (-not $builtinKey) { Fail "no built-in public key in internal/selfupdate/keys.go; pass -PublicKey" }
}

$version = $Tag.Substring(1)
$trusted = "lectureflow update version:$version"
# After minisign ran, every failure deletes what it wrote.
function FailSigned([string]$msg) {
  Remove-Item -LiteralPath $sig -Force -ErrorAction SilentlyContinue
  Fail "$msg (the signature was deleted)"
}
if ((Sha256Of $Manifest) -ne $checked) { Fail "$Manifest changed after the check; nothing was signed" }
Write-Host "Signing $Manifest (LectureFlow $version); minisign asks for the key's password."
& $Minisign -S -s $SecretKey -m $Manifest -x $sig -t $trusted -c "LectureFlow $version update.json"
if ($LASTEXITCODE -ne 0) { FailSigned "minisign failed (exit $LASTEXITCODE); nothing was signed" }
if ((Sha256Of $Manifest) -ne $checked) { FailSigned "$Manifest changed while minisign signed it" }
if (-not (Test-Path -LiteralPath $sig -PathType Leaf)) { FailSigned "minisign wrote no $sig" }
$lines = [System.IO.File]::ReadAllLines((Resolve-Path -LiteralPath $sig).Path)
if ($lines.Count -lt 4 -or $lines[2] -ne "trusted comment: $trusted") {
  FailSigned "$sig does not carry the trusted comment '$trusted'"
}
if ($PublicKey) {
  & $Minisign -V -p $PublicKey -m $Manifest -x $sig
  if ($LASTEXITCODE -ne 0) { FailSigned "the new signature does not verify with $PublicKey" }
} else {
  & $Minisign -V -P $builtinKey -m $Manifest -x $sig
  if ($LASTEXITCODE -ne 0) { FailSigned "the new signature does not verify against $Manifest with the built-in key $builtinKey" }
}
Write-Host "Wrote $sig. Upload it to the draft release, e.g.:"
Write-Host "  gh release upload $Tag $sig --repo OWNER/REPO"
