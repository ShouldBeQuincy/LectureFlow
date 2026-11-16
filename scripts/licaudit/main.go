// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command licaudit is the license audit of SPEC-v3 V7.
//
//	go run ./scripts/licaudit [-root DIR] [-check] [-only deps|spdx] [-out FILE]
//	go run ./scripts/licaudit refresh-python -venv .venv=asr-cuda -venv .venv-ov=asr-openvino
//
// The audit reads the Go module graph and module cache, web/vendor,
// desktop/package-lock.json, the component lock files and the license
// tables in packaging/licenses, judges every license against the
// AGPL-3.0-compatible table (policy.go) and writes THIRD_PARTY_NOTICES.md.
// With -check it writes nothing (CI). It also checks the SPDX header of
// every tracked source file that a release publishes (the tracked files
// that .gitignore does not exclude, as make-release.sh; gitTrackedFiles),
// and that the worker sources point to the additional permission in
// LICENSE-EXCEPTION.md. Any violation exits with status 1.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "refresh-python" {
		os.Exit(refreshPythonMain(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(auditMain(os.Args[1:], os.Stdout, os.Stderr, listGoModules, gitTrackedFiles))
}

func auditMain(args []string, stdout, stderr io.Writer, goMods func(string) ([]GoModule, error), tracked func(string) ([]string, error)) int {
	fs := flag.NewFlagSet("licaudit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	check := fs.Bool("check", false, "only check, do not write the notices file")
	only := fs.String("only", "", "run only one part: deps or spdx")
	out := fs.String("out", "THIRD_PARTY_NOTICES.md", "notices file (relative to -root)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *only != "" && *only != "deps" && *only != "spdx" {
		fmt.Fprintln(stderr, "licaudit: -only must be deps or spdx")
		return 2
	}
	failed := false

	if *only != "spdx" {
		rep, err := Run(Options{Root: *root, GoModules: goMods})
		if err != nil {
			fmt.Fprintln(stderr, "licaudit:", err)
			return 2
		}
		for _, s := range rep.Skipped {
			fmt.Fprintln(stdout, "skipped:", s)
		}
		for _, w := range rep.Warnings {
			fmt.Fprintln(stdout, "warning:", w)
		}
		fmt.Fprintf(stdout, "dependency audit: %d components, %d violations\n", len(rep.Components), len(rep.Violations))
		for _, v := range rep.Violations {
			fmt.Fprintln(stdout, "  FAIL", v)
		}
		if !*check {
			path := filepath.Join(*root, *out)
			if err := writeFileAtomic(path, []byte(RenderNotices(rep))); err != nil {
				fmt.Fprintln(stderr, "licaudit:", err)
				return 2
			}
			fmt.Fprintln(stdout, "wrote", *out)
		}
		failed = failed || len(rep.Violations) > 0
	}

	if *only != "deps" {
		files, err := tracked(*root)
		if err != nil {
			fmt.Fprintln(stderr, "licaudit: listing tracked files:", err)
			return 2
		}
		missing, err := CheckSPDX(*root, files)
		if err != nil {
			fmt.Fprintln(stderr, "licaudit:", err)
			return 2
		}
		fmt.Fprintf(stdout, "SPDX check: %d files without %q in the first %d lines\n", len(missing), spdxTag, spdxHeadLines)
		for _, m := range missing {
			fmt.Fprintln(stdout, "  FAIL", m)
		}
		failed = failed || len(missing) > 0

		noNotice, err := CheckExceptionNotice(*root, files)
		if err != nil {
			fmt.Fprintln(stderr, "licaudit:", err)
			return 2
		}
		fmt.Fprintf(stdout, "worker header check: %d worker sources without %q in the first %d lines\n", len(noNotice), exceptionNotice, spdxHeadLines)
		for _, m := range noNotice {
			fmt.Fprintln(stdout, "  FAIL", m)
		}
		failed = failed || len(noNotice) > 0
	}
	if failed {
		return 1
	}
	return 0
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
