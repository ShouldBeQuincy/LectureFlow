// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// electron-builder configuration (SPEC-v3 V8–V10). Signing material is read
// from the environment only; without it the build is unsigned and does not
// fail (V9):
//
//   Windows  CSC_LINK / CSC_KEY_PASSWORD (or WIN_CSC_LINK / WIN_CSC_KEY_PASSWORD):
//            a .pfx (path, https URL or base64). Absent → unsigned.
//   macOS    CSC_LINK / CSC_KEY_PASSWORD or CSC_NAME: Developer ID Application.
//            Absent → ad-hoc signature ("-"), which Apple Silicon needs to run.
//            Notarization when also APPLE_API_KEY + APPLE_API_KEY_ID +
//            APPLE_API_ISSUER, or APPLE_ID + APPLE_APP_SPECIFIC_PASSWORD +
//            APPLE_TEAM_ID are set.
//
// Updates (V8) only from the repository named at build time
// (scripts/lib/updaterepo.js):
//   LECTUREFLOW_UPDATE_REPO     owner/name; unset → publish: null, no
//                               app-update.yml, the updater stays off
//   LECTUREFLOW_PUBLISHER_NAME  Windows publisher name for verifying update
//                               signatures (signed builds; default: the
//                               certificate's common name)
// A malformed LECTUREFLOW_UPDATE_REPO makes loading this file throw.
'use strict';

const { signingFrom } = require('./scripts/lib/signing');
const { updateBuildConfig } = require('./scripts/lib/updaterepo');
// The minimum macOS (LSMinimumSystemVersion): the highest minimum of Electron,
// the Go programs and the binaries in resources/ (packaging/third-party.json
// macos explains it; scripts/stage.js checks every Mach-O file against it).
const { macos } = require('./packaging/third-party.json');

const { macCert, macNotarize, winCert } = signingFrom(process.env);
const updates = updateBuildConfig(process.env, { winCert });

// Electron fuses (flipped in the packaged binary, SPEC-v3 V2):
const electronFuses = {
  runAsNode: false, // ELECTRON_RUN_AS_NODE cannot turn the app into a Node runtime
  enableNodeOptionsEnvironmentVariable: false, // NODE_OPTIONS is ignored
  enableNodeCliInspectArguments: false, // --inspect / --inspect-brk are ignored
  enableCookieEncryption: true, // cookies of the persistent session encrypted with the OS key store
  enableEmbeddedAsarIntegrityValidation: true, // app.asar must match the hash built into the executable
  onlyLoadAppFromAsar: true, // no app/ folder next to app.asar is ever loaded
  loadBrowserProcessSpecificV8Snapshot: false,
  // The start screen is a local file (src/splash.html); it needs no extra
  // file:// privileges, but the fuse stays at Electron's default until that
  // page has been checked without them on both platforms.
  grantFileProtocolExtraPrivileges: true,
};

module.exports = {
  appId: 'org.lectureflow.desktop', // = AppUserModelId in src/main/main.js
  productName: 'LectureFlow',
  copyright: 'Copyright (C) 2026 LectureFlow contributors',
  directories: { output: 'dist', buildResources: 'build' },
  files: ['package.json', 'src/**/*', 'locales/**/*', '!**/*.map'],
  extraResources: [{ from: 'build/resources', to: '.', filter: ['**/*'] }],
  asar: true,
  npmRebuild: false,
  nodeGypRebuild: false,
  buildDependenciesFromSource: false,
  electronFuses,
  icon: 'build/icon.png',
  // null (no LECTUREFLOW_UPDATE_REPO): no update feed at all. electron-builder
  // would otherwise guess a GitHub repository from package.json, the git
  // remote or GH_TOKEN and write it into app-update.yml.
  publish: updates.publish,
  win: {
    target: [
      { target: 'nsis', arch: ['x64'] },
      { target: 'portable', arch: ['x64'] },
    ],
    executableName: 'LectureFlow',
    // Signed builds sign every .exe of extraResources too; aria2c.exe is
    // upstream's official build whose SHA-256 is pinned (stage.js,
    // components.json), so it stays byte for byte as published. Only the
    // exclusion: a positive entry (".exe") would win over it (electron-builder
    // 26 shouldSignFile; NOTES.md "Windows 签名排除 aria2c.exe").
    signExts: ['!aria2c.exe'],
    verifyUpdateCodeSignature: true, // signed builds: publisherName goes into app-update.yml
    ...(updates.winPublisherName ? { signtoolOptions: { publisherName: updates.winPublisherName } } : {}),
  },
  nsis: {
    oneClick: false,
    perMachine: false, // per user, no administrator rights
    allowElevation: false,
    allowToChangeInstallationDirectory: true,
    createDesktopShortcut: true,
    createStartMenuShortcut: true,
    deleteAppDataOnUninstall: false, // the data directory belongs to the user
    // Uninstall removes the browser extension's native messaging
    // registration (HKCU keys, manifests, the host copy); see the file.
    include: 'packaging/installer.nsh',
    artifactName: 'LectureFlow-Setup-${version}-x64.${ext}',
  },
  portable: {
    artifactName: 'LectureFlow-${version}-x64-portable.${ext}',
  },
  mac: {
    target: [
      { target: 'dmg', arch: ['arm64'] },
      { target: 'zip', arch: ['arm64'] }, // Squirrel.Mac (updates) needs the zip next to the dmg
    ],
    category: 'public.app-category.education',
    identity: macCert ? undefined : '-',
    hardenedRuntime: true,
    gatekeeperAssess: false,
    entitlements: 'packaging/entitlements.mac.plist',
    entitlementsInherit: 'packaging/entitlements.mac.plist',
    notarize: macNotarize,
    minimumSystemVersion: macos.minimum_system_version,
    // ssh-1b: the SSH servers' agent for Linux and Windows (resources/agent,
    // scripts/stage.js) is no macOS code and is left unsigned; the
    // darwin-arm64 one is signed like every Mach-O file of the app.
    signIgnore: ['/Contents/Resources/agent/(linux-x64|windows-x64)/'],
  },
  dmg: {
    artifactName: 'LectureFlow-${version}-arm64.${ext}',
  },
};
