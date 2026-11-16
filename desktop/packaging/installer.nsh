# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

# NSIS additions (electron-builder nsis.include). The app registers the
# browser extension's native messaging host at every start (internal/nmhost):
# three HKCU keys, the manifests and launch.json in %APPDATA%\LectureFlow\nmhost,
# and for the portable build a copy of the host in %LOCALAPPDATA%\LectureFlow\nmhost.
# The in-app update keeps its record of the installation and the backups in
# %LOCALAPPDATA%\LectureFlow\selfupdate (desktop/src/main/lib/updatefiles.js
# trustDirFor). Uninstalling removes them. Not during an update: the old
# version's uninstaller runs then — it must not take the record of the
# update in progress with it — and the new version registers again at its
# start. The data directory is never touched (deleteAppDataOnUninstall: false).
# The portable build has no uninstaller: its small record stays.

# The installation folder's permissions (security audit M-4). The installer
# runs per user, without administrator rights, and the user may choose any
# folder. Inside the user's profile or %LOCALAPPDATA% (the default is
# %LOCALAPPDATA%\Programs\lectureflow) a new folder admits only the user,
# SYSTEM and the administrators. Anywhere else it takes the permissions of
# the folder it is made in: on a drive such as D:\ every signed-in account
# may then change it and replace the programs the user starts. So such a
# folder gets the private permissions of LectureFlow's data folder
# (internal/dirperm): a protected DACL (nothing inherited from above) that
# gives full control to the user, SYSTEM and Administrators and is passed
# down to everything in it —
#   icacls <folder> /inheritance:r /grant:r *<user SID>:(OI)(CI)F *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F
# — at every installation, updates included (an update's uninstaller
# removes the old files, and the folder it may remove with them comes back
# with its parent's permissions; the same permissions are set again).
# Entries a folder that already existed had of its own are left as they
# are. The root of a drive and network folders are never changed, nor is an
# installation "for all users" (an administrator running the installer, or
# /allusers: Program Files, whose permissions let every account run the
# app and no other account change it; security review SEC20). When
# nothing can be set (FAT and exFAT keep no permissions; the user's SID
# cannot be read) the installation goes on: the app's diagnostics then say
# that other accounts can change the folder, and in-app updates stay off
# (internal/selfupdate/installdir.go). Only icacls from the system folder
# runs, never a program from the installation folder (other accounts could
# have changed it before its permissions were set). A known limitation
# (security review SEC20, medium 2): the folder is tightened only after the files
# were unpacked, and during an in-app update the old version's uninstaller
# removes it and this installer makes it again, with its parent's
# permissions until then; making it privately before unpacking would need
# customUnInstallCheck / customRemoveFiles, which are not changed without a
# way to run the installer. A folder of that name another account made
# before the first installation falls under the same limitation. After a
# failed update is rolled back, the app makes the installation folder anew
# and it takes its permissions from the folder above: outside the default
# place the private permissions do not come back by themselves (the app
# checks the folder and says so; the user installs again or tightens it),
# and while the rollback copies the files, they can be changed by the
# other accounts that may change the folder above.
# No variables or
# functions of its own (electron-builder treats NSIS warnings as errors):
# $R0-$R6, saved and restored.

# lfInside DIR BASE OUT: OUT is 1 when the full path DIR is BASE or lies in
# it (StrCmp ignores case; the backslash keeps "Local2" out of "Local"),
# else 0. Uses $R5 and $R6.
!macro lfInside DIR BASE OUT
  StrCpy ${OUT} 0
  ${if} "${BASE}" != ""
    GetFullPathName $R5 "${BASE}"
    ${if} $R5 == ""
      StrCpy $R5 "${BASE}"
    ${endIf}
    StrLen $R6 "$R5\"
    StrCpy $R6 "${DIR}\" $R6
    ${if} $R6 == "$R5\"
      StrCpy ${OUT} 1
    ${endIf}
  ${endIf}
!macroend

!macro lfProtectInstallDir
  Push $R0
  Push $R1
  Push $R2
  Push $R3
  Push $R4
  Push $R5
  Push $R6
  GetFullPathName $R0 "$INSTDIR"
  ${if} $R0 == ""
    StrCpy $R0 "$INSTDIR"
  ${endIf}
  StrCpy $R2 $R0 1 -1
  StrLen $R3 $R0
  ${if} $R2 == "\"
  ${andIf} $R3 > 3
    StrCpy $R0 $R0 -1
  ${endIf}
  StrCpy $R2 $R0 2
  StrLen $R3 $R0
  !insertmacro lfInside $R0 "$PROFILE" $R1
  !insertmacro lfInside $R0 "$LOCALAPPDATA" $R4
  ${if} $installMode == "all"
    DetailPrint "LectureFlow: installed for all users ($R0); its permissions are left as they are"
  ${elseIf} $R3 <= 3
  ${orIf} $R2 == "\\"
    DetailPrint "LectureFlow: $R0 is the root of a drive or a network folder; its permissions are left as they are"
  ${elseIf} $R1 == 1
  ${orIf} $R4 == 1
    DetailPrint "LectureFlow: $R0 lies in the user's profile and keeps its permissions"
  ${else}
    # the user's SID (TOKEN_USER of this process's token) as a string
    # (<> compares numbers: a call that failed or could not be made, "error"
    # included, counts as 0)
    StrCpy $R1 ""
    System::Call 'kernel32::GetCurrentProcess() p .R3'
    System::Call 'advapi32::OpenProcessToken(p R3, i 0x8, *p .R3) i .R2'
    ${if} $R2 <> 0
      System::Call 'advapi32::GetTokenInformation(p R3, i 1, p 0, i 0, *i .R4)'
      ${if} $R4 > 0
        System::Alloc $R4
        Pop $R5
        System::Call 'advapi32::GetTokenInformation(p R3, i 1, p R5, i R4, *i .R4) i .R2'
        ${if} $R2 <> 0
          System::Call '*$R5(p .R6)'
          System::Call 'advapi32::ConvertSidToStringSidW(p R6, *p .R4) i .R2'
          ${if} $R2 <> 0
            System::Call 'kernel32::lstrcpynW(w .R1, p R4, i ${NSIS_MAX_STRLEN})'
            System::Call 'kernel32::LocalFree(p R4)'
          ${endIf}
        ${endIf}
        System::Free $R5
      ${endIf}
      System::Call 'kernel32::CloseHandle(p R3)'
    ${endIf}
    StrCpy $R2 $R1 4
    ${if} $R2 != "S-1-"
      DetailPrint "LectureFlow: the user's SID could not be read; the permissions of $R0 are left as they are"
    ${else}
      nsExec::ExecToLog '"$SYSDIR\icacls.exe" "$R0" /inheritance:r /grant:r "*$R1:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" /Q'
      Pop $R2
      ${if} $R2 == 0
        DetailPrint "LectureFlow: only this account, SYSTEM and Administrators can change $R0"
      ${else}
        DetailPrint "LectureFlow: the permissions of $R0 could not be set (icacls: $R2); other accounts may be able to change it"
      ${endIf}
    ${endIf}
  ${endIf}
  Pop $R6
  Pop $R5
  Pop $R4
  Pop $R3
  Pop $R2
  Pop $R1
  Pop $R0
!macroend

!macro customInstall
  !insertmacro lfProtectInstallDir
!macroend

!macro customUnInstall
  ${ifNot} ${isUpdated}
    DeleteRegKey HKCU "Software\Google\Chrome\NativeMessagingHosts\org.lectureflow.nmhost"
    DeleteRegKey HKCU "Software\Microsoft\Edge\NativeMessagingHosts\org.lectureflow.nmhost"
    DeleteRegKey HKCU "Software\Mozilla\NativeMessagingHosts\org.lectureflow.nmhost"
    RMDir /r "$APPDATA\LectureFlow\nmhost"
    RMDir /r "$LOCALAPPDATA\LectureFlow\nmhost"
    RMDir /r "$LOCALAPPDATA\LectureFlow\selfupdate"
  ${endIf}
!macroend
