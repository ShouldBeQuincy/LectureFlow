// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SPDX header check (SPEC-v3 V7): every tracked source file must carry
// "SPDX-License-Identifier: AGPL-3.0-or-later" in its first six lines.
//
// The identifier stays plain AGPL-3.0-or-later, also in the files that the
// additional permission concerns: SPDX 2.x accepts only exceptions from the
// SPDX list after WITH, and the permission (LICENSE-EXCEPTION.md) applies
// to LectureFlow as a whole, so marking only some files with an SPDX 3
// "WITH AdditionRef-" would suggest that the others lack it. Instead, the
// Python sources of the transcription workers, which load the libraries
// that the permission names, carry exceptionNotice as well
// (CheckExceptionNotice; user decision 2026-10-27).

const (
	spdxTag         = "SPDX-License-Identifier: AGPL-3.0-or-later"
	spdxHeadLines   = 6
	exceptionNotice = "Additional permission under AGPL-3.0 section 7: see " + exceptionFile
)

var spdxExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true, ".css": true,
	".html": true, ".sql": true, ".ps1": true, ".sh": true,
}

// spdxExcluded: third-party code, fixtures and documentation. The
// extension's vendor directory holds third-party files only; the dependency
// audit fails on any file there that packaging/licenses/extension.json does
// not register (extension.go).
func spdxExcluded(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, p := range []string{"web/vendor/", "docs/", "desktop/node_modules/", extVendorDir + "/"} {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "testdata" || seg == "node_modules" {
			return true
		}
	}
	return false
}

// gitTrackedFiles lists the files that a release publishes (relative, slash
// form): the files tracked by git (git ls-files) minus those that are
// tracked but match an ignore rule (git ls-files -ci --exclude-standard).
// .claude/release/make-release.sh removes exactly that second set from the
// published tree; these are the agent and process files that the
// development history keeps (.agent-log/, .claude/, PROGRESS.md, spikes/
// and the like; see .gitignore), so the header checks look at the files
// that are published and at nothing else. On a published tree, where no
// tracked file is ignored, the set is the whole of git ls-files, as before.
//
// When git is missing or root is not inside a work tree, git ls-files
// fails and so does this function; the audit exits with status 2, as it
// did before the ignored files were left out. The same holds should the
// second call fail: no silent fallback to a set that would skip files.
func gitTrackedFiles(root string) ([]string, error) {
	tracked, err := gitLsFiles(root)
	if err != nil {
		return nil, err
	}
	ignored, err := gitLsFiles(root, "-ci", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	skip := make(map[string]bool, len(ignored))
	for _, f := range ignored {
		skip[f] = true
	}
	var files []string
	for _, f := range tracked {
		if !skip[f] {
			files = append(files, f)
		}
	}
	return files, nil
}

// gitLsFiles runs git ls-files -z with the extra arguments in root.
func gitLsFiles(root string, args ...string) ([]string, error) {
	cmd := exec.Command("git", append([]string{"ls-files", "-z"}, args...)...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

// CheckSPDX returns the source files (relative paths) without the header.
func CheckSPDX(root string, files []string) ([]string, error) {
	return checkHeads(root, files, func(rel string) bool {
		return spdxExts[strings.ToLower(filepath.Ext(rel))] && !spdxExcluded(rel)
	}, spdxTag)
}

// needsExceptionNotice: the Python sources of worker/, without the tests
// (conftest.py, test_*.py, *_test.py), which are not part of the workers
// that LectureFlow ships.
func needsExceptionNotice(rel string) bool {
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, "worker/") || strings.ToLower(path.Ext(rel)) != ".py" || spdxExcluded(rel) {
		return false
	}
	base := strings.ToLower(path.Base(rel))
	return base != "conftest.py" && !strings.HasPrefix(base, "test_") && !strings.HasSuffix(base, "_test.py")
}

// CheckExceptionNotice returns the worker sources (relative paths) without
// exceptionNotice in their first lines.
func CheckExceptionNotice(root string, files []string) ([]string, error) {
	return checkHeads(root, files, needsExceptionNotice, exceptionNotice)
}

// checkHeads returns the files selected by want whose first spdxHeadLines
// lines do not contain tag. Files deleted in the working tree are skipped.
func checkHeads(root string, files []string, want func(string) bool, tag string) ([]string, error) {
	var missing []string
	for _, rel := range files {
		if !want(rel) {
			continue
		}
		ok, err := headContains(filepath.Join(root, filepath.FromSlash(rel)), tag)
		if os.IsNotExist(err) {
			continue // deleted in the working tree
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, filepath.ToSlash(rel))
		}
	}
	sort.Strings(missing)
	return missing, nil
}

func headContains(file, tag string) (bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, 16*1024)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false, err
	}
	lines := strings.SplitN(string(head[:n]), "\n", spdxHeadLines+1)
	if len(lines) > spdxHeadLines {
		lines = lines[:spdxHeadLines]
	}
	for _, l := range lines {
		if strings.Contains(l, tag) {
			return true, nil
		}
	}
	return false, nil
}
