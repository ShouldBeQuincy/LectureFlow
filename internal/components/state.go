// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	stateFileName   = "state.json"
	receiptFileName = "installed.json"
	stateSchema     = 1
)

// Installed is one installed component version (state.json entry and the
// receipt in its directory). Dir is relative to the components directory
// ("/"-separated), so the data directory can move.
type Installed struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Kind        string `json:"kind"`
	Backend     string `json:"backend,omitempty"`
	Platform    string `json:"platform"`
	Dir         string `json:"dir"`
	InstalledAt string `json:"installed_at"`
	SizeBytes   int64  `json:"size_bytes"`
	// Python is the python component version a venv was built with.
	Python string `json:"python,omitempty"`
	// Files are the downloaded files (model, binary, the runtime archive):
	// their sha256 lets a later install reuse them.
	Files []InstalledFile `json:"files,omitempty"`
	// Exe is the runtime interpreter (python component) or the tool's
	// executable (tool components), relative to Dir.
	Exe string `json:"exe,omitempty"`
	// Env are the variable templates of a tool (Build.Env when it was
	// installed; ToolEnv expands them).
	Env map[string]string `json:"env,omitempty"`
}

// InstalledFile is a file of an Installed component (Path relative to its Dir).
type InstalledFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// state is state.json.
type state struct {
	Schema int `json:"schema"`
	// Active maps id → the version in use.
	Active map[string]Installed `json:"active"`
	// Retained are older python runtimes still used by venvs built with
	// them (removed once no installed venv refers to them).
	Retained []Installed `json:"retained,omitempty"`
}

func newState() *state { return &state{Schema: stateSchema, Active: map[string]Installed{}} }

func (s *state) clone() *state {
	c := &state{Schema: s.Schema, Active: make(map[string]Installed, len(s.Active))}
	for k, v := range s.Active {
		c.Active[k] = v
	}
	c.Retained = append([]Installed(nil), s.Retained...)
	return c
}

// referencedDirs are the directories state.json keeps.
func (s *state) referencedDirs() map[string]bool {
	out := map[string]bool{}
	for _, e := range s.Active {
		out[e.Dir] = true
	}
	for _, e := range s.Retained {
		out[e.Dir] = true
	}
	return out
}

// writeFileAtomic writes data to a temporary file in the same directory,
// syncs it and renames it over path.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = renameRetry(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// renameRetry renames, retrying briefly: on Windows a scanner or indexer
// holding a fresh file makes rename fail for a moment.
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return err
		}
		time.Sleep(time.Duration(25*(i+1)) * time.Millisecond)
	}
	return err
}

// removeAllRetry removes a tree, retrying briefly for the same reason.
func removeAllRetry(path string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(25*(i+1)) * time.Millisecond)
	}
	return err
}

func marshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (m *Manager) writeState(s *state) error {
	b, err := marshalIndent(s)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(m.root, stateFileName), b); err != nil {
		return cerr(CodeIO, nil, fmt.Errorf("write %s: %w", stateFileName, err))
	}
	return nil
}

// loadState reads state.json. Missing → rebuilt from the receipts (a fresh
// directory has none). Unreadable or invalid → moved aside to
// state.json.corrupt-{time} and rebuilt from the receipts; recovered
// reports that case.
func (m *Manager) loadState() (s *state, recovered bool, err error) {
	p := filepath.Join(m.root, stateFileName)
	data, rerr := os.ReadFile(p)
	if rerr == nil {
		s = &state{}
		if jerr := json.Unmarshal(data, s); jerr == nil && s.Schema == stateSchema && s.Active != nil && s.valid() {
			return s, false, nil
		} else {
			if jerr == nil {
				jerr = errors.New("unexpected content")
			}
			aside := fmt.Sprintf("%s.corrupt-%d", p, m.now().UnixNano())
			m.log.Warn("components: state.json is damaged; rebuilding it from the receipts",
				"error", jerr, "moved_to", aside)
			if err := renameRetry(p, aside); err != nil {
				return nil, false, cerr(CodeIO, nil, err)
			}
			recovered = true
		}
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return nil, false, cerr(CodeIO, nil, rerr)
	}
	s = m.scanReceipts()
	if len(s.Active) > 0 || recovered {
		if err := m.writeState(s); err != nil {
			return nil, recovered, err
		}
	}
	return s, recovered, nil
}

// exeOK checks an entry's Exe (security audit L-13): a "/"-separated path
// inside its directory (safeRelPath: no absolute path, drive, UNC path,
// backslash or ".."), required for the python runtime and tools, absent or
// equally safe for the other kinds. An entry that fails it is treated like
// a damaged receipt and never run.
func exeOK(e Installed) bool {
	switch e.Kind {
	case KindPythonRuntime, KindTool:
		return safeRelPath(e.Exe)
	}
	return e.Exe == "" || safeRelPath(e.Exe)
}

// filesOK checks an entry's Files[].Path like Exe (inside its directory);
// the manager stats them (intact) and reads them to reuse their content.
func filesOK(e Installed) bool {
	for _, f := range e.Files {
		if !safeRelPath(f.Path) {
			return false
		}
	}
	return true
}

// filePath is the absolute path of an entry's file; false when Path is not
// a safe relative path.
func (m *Manager) filePath(e Installed, f InstalledFile) (string, bool) {
	if !safeRelPath(f.Path) || !safeRelPath(e.Dir) {
		return "", false
	}
	return filepath.Join(m.abs(e.Dir), filepath.FromSlash(f.Path)), true
}

// exePath is the absolute path of an entry's Exe; false when it has none
// or it is not a safe relative path.
func (m *Manager) exePath(e Installed) (string, bool) {
	if e.Exe == "" || !safeRelPath(e.Exe) || !safeRelPath(e.Dir) {
		return "", false
	}
	return filepath.Join(m.abs(e.Dir), filepath.FromSlash(e.Exe)), true
}

func (s *state) valid() bool {
	check := func(e Installed) bool {
		return e.ID != "" && e.Version != "" && safeRelPath(e.Dir) && exeOK(e) && filesOK(e)
	}
	for id, e := range s.Active {
		if id != e.ID || !check(e) {
			return false
		}
	}
	for _, e := range s.Retained {
		if !check(e) {
			return false
		}
	}
	return true
}

// scanReceipts rebuilds the state from {dir}/{version}/installed.json:
// per component the highest version with a valid receipt whose directory
// matches. A receipt is written only after every check passed, so every
// receipt marks a complete installation.
func (m *Manager) scanReceipts() *state {
	s := newState()
	entries, _ := os.ReadDir(m.root)
	for _, ce := range entries {
		if !ce.IsDir() || strings.HasPrefix(ce.Name(), ".") || isReservedDir(ce.Name()) {
			continue
		}
		vers, _ := os.ReadDir(filepath.Join(m.root, ce.Name()))
		for _, ve := range vers {
			if !ve.IsDir() || strings.HasPrefix(ve.Name(), ".") {
				continue
			}
			rel := ce.Name() + "/" + ve.Name()
			r, err := readReceipt(filepath.Join(m.root, ce.Name(), ve.Name()))
			if err != nil || r.Dir != rel || DirName(r.ID) != ce.Name() || r.Platform != m.platform {
				continue
			}
			if cur, ok := s.Active[r.ID]; !ok || CompareVersions(r.Version, cur.Version) > 0 {
				s.Active[r.ID] = r
			}
		}
	}
	// keep the runtimes the recovered venvs were built with
	for _, e := range s.Active {
		if e.Kind != KindVenv || e.Python == "" {
			continue
		}
		if py, ok := s.Active[IDPython]; ok && py.Version == e.Python {
			continue
		}
		dir := DirName(IDPython) + "/" + e.Python
		if r, err := readReceipt(filepath.Join(m.root, filepath.FromSlash(dir))); err == nil && r.Dir == dir {
			s.Retained = appendUnique(s.Retained, r)
		}
	}
	return s
}

func appendUnique(list []Installed, e Installed) []Installed {
	for _, x := range list {
		if x.Dir == e.Dir {
			return list
		}
	}
	return append(list, e)
}

func isReservedDir(name string) bool {
	switch name {
	case "downloads", "manifest.d":
		return true
	}
	return false
}

func readReceipt(dir string) (Installed, error) {
	var r Installed
	data, err := os.ReadFile(filepath.Join(dir, receiptFileName))
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if r.ID == "" || r.Version == "" || !safeRelPath(r.Dir) || !exeOK(r) || !filesOK(r) {
		return r, errors.New("incomplete receipt")
	}
	return r, nil
}

func writeReceipt(dir string, r Installed) error {
	b, err := marshalIndent(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, receiptFileName), b)
}

// cleanOrphans removes what an interrupted run left behind: staging
// directories, version directories state.json does not reference, and
// temporary files of atomic writes. Partial downloads stay (they resume).
func (m *Manager) cleanOrphans(s *state) {
	keep := s.referencedDirs()
	entries, _ := os.ReadDir(m.root)
	for _, ce := range entries {
		name := ce.Name()
		if !ce.IsDir() {
			if strings.HasPrefix(name, "."+stateFileName+".tmp-") || strings.HasPrefix(name, ".manifest.json.tmp-") {
				os.Remove(filepath.Join(m.root, name))
			}
			continue
		}
		if strings.HasPrefix(name, ".") || isReservedDir(name) {
			continue
		}
		vers, _ := os.ReadDir(filepath.Join(m.root, name))
		for _, ve := range vers {
			rel := name + "/" + ve.Name()
			if keep[rel] {
				continue
			}
			p := filepath.Join(m.root, name, ve.Name())
			m.log.Info("components: removing an incomplete or unused installation", "dir", rel)
			if err := removeAllRetry(p); err != nil {
				m.log.Warn("components: cannot remove", "dir", rel, "error", err)
			}
		}
		if rest, _ := os.ReadDir(filepath.Join(m.root, name)); len(rest) == 0 {
			os.Remove(filepath.Join(m.root, name))
		}
	}
}

// dirSize sums the sizes of the regular files under dir.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// sortedIDs returns the keys of the active map in order.
func (s *state) sortedIDs() []string {
	ids := make([]string, 0, len(s.Active))
	for id := range s.Active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
