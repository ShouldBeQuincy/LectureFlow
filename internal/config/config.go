// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package config loads config.toml and applies defaults (SPEC §2, §17).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"lectureflow/internal/model"
)

type Config struct {
	Listen    string `toml:"listen"`
	DataDir   string `toml:"data_dir"`
	Python    string `toml:"python"`
	FFmpeg    string `toml:"ffmpeg"`
	NvidiaSMI string `toml:"nvidia_smi"`
	LogLevel  string `toml:"log_level"` // info | debug
	// WorkerDir is the directory holding asr.py / extract_pdf.py.
	WorkerDir string `toml:"worker_dir"`
	// PythonOpenVINO is the interpreter of worker/asr_openvino.py (SPEC-v2
	// I7): the separate .venv-ov environment. Default
	// .venv-ov\Scripts\python.exe (.venv-ov/bin/python elsewhere). When it
	// does not exist, OpenVINO devices are simply not detected.
	//
	// Python, PythonOpenVINO and PythonMLX get their development defaults
	// (.venv, .venv-ov, .venv-mlx of the working directory) only without
	// ResourcesDir. In the desktop application (ResourcesDir set) the
	// interpreters are the components' (WorkerPython); a key config.toml
	// does not set stays empty there, which means "not installed" (3.1.0:
	// the desktop shell starts the service in the resources directory, so
	// the defaults named Resources/.venv, which never exists).
	PythonOpenVINO string `toml:"python_openvino"`
	// ResourcesDir is the packaged resources directory of the desktop
	// application (command-line -resources, SPEC-v3 V1, v3-brief §1):
	// DIR/worker/, DIR/bin/ffmpeg(.exe), DIR/bin/ffprobe(.exe),
	// DIR/bin/uv(.exe), DIR/bin/aria2c(.exe), DIR/components.json. Empty
	// in development. When set, WorkerDir, FFmpeg, UV and Aria2c are taken
	// from it where the file exists (applyResources).
	ResourcesDir string `toml:"resources_dir"`
	// UV is the uv executable (DIR/bin/uv(.exe) of ResourcesDir when it
	// exists); empty = not available.
	UV string `toml:"uv"`
	// Aria2c is the aria2c executable of the download queue (SPEC-ext
	// "内建 aria2c", ext-brief §6.1): DIR/bin/aria2c(.exe) of ResourcesDir
	// when it exists (bundled with the installer), else the configured
	// value, else "aria2c" from PATH.
	Aria2c string `toml:"aria2c"`
	// PythonMLX is the interpreter of worker/asr_mlx.py (SPEC-v3 V5): the
	// separate .venv-mlx environment. Default .venv-mlx/bin/python
	// (.venv-mlx\Scripts\python.exe on Windows, for symmetry only). When it
	// does not exist, the mlx device is not detected. The asr-mlx
	// component's interpreter takes precedence when it is installed
	// (WorkerPython).
	PythonMLX string `toml:"python_mlx"`
	// SampleDir holds the bundled sample lecture (SPEC-v3 V4.3:
	// sample-lecture.flac, reference.txt) used by the device self-test when
	// no lecture can give a clip. Resolved by LoadWith: the configured
	// value, else resources/sample when ResourcesDir has the sample, else
	// assets/sample of the working directory (development); empty when the
	// audio file does not exist there.
	SampleDir string `toml:"sample_dir"`
	// ComponentsManifest is the built-in component manifest (SPEC-v3 V3.2,
	// components.json; its lock files are relative to its directory).
	// Resolved by LoadWith: resources/components.json when ResourcesDir has
	// it (taking precedence over the configured value, like WorkerDir), else
	// the configured value, else packaging/components/components.json of
	// the working directory (development); empty when the file does not
	// exist (the application then runs without a component manager).
	ComponentsManifest string `toml:"components_manifest"`
	// ComponentsManifestURL is where a newer component manifest is fetched
	// (V3.2, V8); "{channel}" is replaced by the update channel (stable |
	// beta, kv app.update_channel). Empty (the default: the project has no
	// release address yet) = no network check.
	ComponentsManifestURL string `toml:"components_manifest_url"`
	// Components locates what the component manager installed (SPEC-v3 V3,
	// step 5): cmd sets it to the *components.Manager before anything
	// runs. nil = only the configured interpreters and model locations
	// (development .venv / .venv-ov / .venv-mlx, tests). Never read from
	// config.toml. See components.go.
	Components ComponentLocator `toml:"-"`

	// devPython records the interpreter keys ("python", "python_openvino",
	// "python_mlx") applyDefaults filled with the development default, with
	// the value as resolved (absPaths); see PythonSource. Never read from
	// config.toml.
	devPython map[string]string

	ASR     ASR     `toml:"asr"`
	LLM     LLM     `toml:"llm"`
	Storage Storage `toml:"storage"`
}

type ASR struct {
	Model         string `toml:"model"`
	ComputeType   string `toml:"compute_type"` // auto | float16 | int8_float16
	Device        string `toml:"device"`       // cuda | cpu
	Language      string `toml:"language"`
	VRAMPerJobMB  int    `toml:"vram_per_job_mb"`
	MaxParallel   int    `toml:"max_parallel"`
	VRAMReserveMB int    `toml:"vram_reserve_mb"`
	// FakeResult, when non-empty, makes the asr stage read this file instead
	// of running Python (SPEC §14).
	FakeResult string `toml:"fake_result"`
	// FakeResultOpenVINO is FakeResult for jobs whose snapshot backend is
	// openvino (SPEC-v2 I11; testdata/fake_asr_openvino.json: no words, no
	// low_conf_words). Empty = such jobs read FakeResult as well. Only
	// meaningful together with FakeResult (test mode).
	FakeResultOpenVINO string `toml:"fake_result_openvino"`
	// WorkerMemBudgetMB is the host-memory budget of the transcription
	// workers that run at the same time (power management freeze,
	// v2-step20; CLAUDE.md revision 4: the application stays within about
	// 8 GB): the scheduler admits a pipeline asr job only when the observed
	// peaks (mem_observed; devices.MemEstimateMB without one) of the
	// running workers plus the new one stay within it, otherwise the job
	// waits "memory" (task P1). A startup setting like python_openvino
	// (never imported into the settings). Default DefaultWorkerMemBudgetMB.
	WorkerMemBudgetMB int `toml:"worker_mem_budget_mb"`
}

// DefaultWorkerMemBudgetMB is ASR.WorkerMemBudgetMB's default (8 GB).
const DefaultWorkerMemBudgetMB = 8192

// DefaultVRAMPerJobMB is vram_per_job_mb's default (SPEC §2 / §17.2; the
// only default it ever had, config.example.toml too). Since r2 T-PERF-02
// a stored value equal to it counts as "not chosen": the per-model
// estimates apply (devices.VRAMBaseMB).
const DefaultVRAMPerJobMB = 5000

type Provider struct {
	BaseURL           string `toml:"base_url"`
	APIKey            string `toml:"api_key"`
	APIKeyEnv         string `toml:"api_key_env"`
	MaxConcurrency    int    `toml:"max_concurrency"`
	RequestsPerMinute int    `toml:"requests_per_minute"`
	Local             bool   `toml:"local"`
	VRAMMB            int    `toml:"vram_mb"`
}

// Key returns the API key from the config file or the environment.
func (p Provider) Key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return ""
}

type Role struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	Window   int    `toml:"window"` // translate only
}

type Price struct {
	Input    float64 `toml:"input"`
	CacheHit float64 `toml:"cache_hit"`
	Output   float64 `toml:"output"`
}

type LLM struct {
	Providers map[string]Provider `toml:"providers"`
	Roles     map[string]Role     `toml:"roles"`
	// Prices[provider][model], per million tokens (SPEC §17.7).
	Prices      map[string]map[string]Price `toml:"prices"`
	DailyBudget float64                     `toml:"daily_budget"` // 0 = no budget
}

type Storage struct {
	KeepOriginalMedia *bool `toml:"keep_original_media"`
}

func (s Storage) KeepMedia() bool { return s.KeepOriginalMedia == nil || *s.KeepOriginalMedia }

// LLM role names (SPEC §2 [llm.roles]); aliases of the model constants (v1.1).
const (
	RoleGlossary  = model.RoleGlossary
	RoleSummary   = model.RoleSummary
	RoleRepair    = model.RoleRepair
	RoleTranslate = model.RoleTranslate
)

// DefaultDeepSeekModel is the model of the legacy roles when config.toml
// names none (SPEC-v2 P12.1: DeepSeek's current name; deepseek-chat is the
// old one).
const DefaultDeepSeekModel = "deepseek-flash"

// DeepSeekMaxConcurrency is the default max_concurrency of a DeepSeek
// provider (perf-llm, NOTES.md): DeepSeek limits concurrent requests per
// account (2500 for deepseek-flash) and has no per-minute limit; 64 at a
// time translates a lecture in seconds and leaves the account room for the
// user's other programs.
const DeepSeekMaxConcurrency = 64

// Load reads path (missing file = all defaults) and applies defaults.
func Load(path string) (*Config, error) {
	return LoadWith(path, Overrides{})
}

// Overrides are the command-line values that take precedence over the
// config file (v3-brief §1: -listen, -data-dir, -resources). Empty fields
// leave the file's value (or the default) alone.
type Overrides struct {
	Listen       string
	DataDir      string
	ResourcesDir string
}

// LoadWith is Load with the command-line overrides applied over the file,
// before the defaults, the resources directory, validation and absPaths.
func LoadWith(path string, ov Overrides) (*Config, error) {
	c := &Config{}
	if path != "" {
		_, err := toml.DecodeFile(path, c)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}
	if ov.Listen != "" {
		c.Listen = ov.Listen
	}
	if ov.DataDir != "" {
		c.DataDir = ov.DataDir
	}
	if ov.ResourcesDir != "" {
		c.ResourcesDir = ov.ResourcesDir
	}
	c.applyDefaults()
	c.applyResources()
	if c.SampleDir == "" {
		c.SampleDir = DevSampleDir
	}
	if c.ComponentsManifest == "" {
		c.ComponentsManifest = DevComponentsManifest
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := c.absPaths(); err != nil {
		return c, err
	}
	if !isFile(filepath.Join(c.SampleDir, SampleAudioFile)) {
		c.SampleDir = ""
	}
	if !isFile(c.ComponentsManifest) {
		c.ComponentsManifest = ""
	}
	return c, nil
}

// SampleAudioFile is the bundled sample's audio file inside SampleDir
// (devices.BundledAudioFile).
const SampleAudioFile = "sample-lecture.flac"

// DevSampleDir is where the sample lives in the repository (relative to
// the working directory; development runs from the repository root).
var DevSampleDir = filepath.Join("assets", "sample")

// applyResources takes the worker directory, ffmpeg, uv and aria2c from
// ResourcesDir where they exist (v3-brief §1). A missing file keeps the
// configured value or the default (development: ffmpeg from PATH, worker/
// of the repository). ffprobe needs no field: the stages look for it next
// to ffmpeg.
func (c *Config) applyResources() {
	if c.ResourcesDir == "" {
		return
	}
	if fi, err := os.Stat(filepath.Join(c.ResourcesDir, "worker")); err == nil && fi.IsDir() {
		c.WorkerDir = filepath.Join(c.ResourcesDir, "worker")
	}
	if p := filepath.Join(c.ResourcesDir, "bin", exe("ffmpeg")); isFile(p) {
		c.FFmpeg = p
	}
	if p := filepath.Join(c.ResourcesDir, "bin", exe("uv")); isFile(p) {
		c.UV = p
	}
	if p := filepath.Join(c.ResourcesDir, "bin", exe("aria2c")); isFile(p) {
		c.Aria2c = p
	}
	if dir := filepath.Join(c.ResourcesDir, "sample"); isFile(filepath.Join(dir, SampleAudioFile)) {
		c.SampleDir = dir
	}
	if p := filepath.Join(c.ResourcesDir, ComponentsManifestFile); isFile(p) {
		c.ComponentsManifest = p
	}
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// absPaths resolves relative paths against the current working directory.
// Subprocesses (ffmpeg, Python workers) run with the lecture or material
// directory as their working directory (SPEC §6.0), so a relative data_dir
// would point to the wrong place there.
func (c *Config) absPaths() error {
	var err error
	abs := func(p *string) {
		if err != nil || *p == "" || filepath.IsAbs(*p) {
			return
		}
		*p, err = filepath.Abs(*p)
	}
	abs(&c.DataDir)
	abs(&c.WorkerDir)
	abs(&c.ResourcesDir)
	abs(&c.SampleDir)
	abs(&c.ComponentsManifest)
	abs(&c.ASR.FakeResult)
	abs(&c.ASR.FakeResultOpenVINO)
	// Executables given with a directory part are paths; bare names are
	// looked up in PATH and stay as they are.
	for _, p := range []*string{&c.Python, &c.PythonOpenVINO, &c.FFmpeg, &c.NvidiaSMI, &c.UV, &c.PythonMLX, &c.Aria2c} {
		if filepath.Base(*p) != *p {
			abs(p)
		}
	}
	for key := range c.devPython {
		c.devPython[key] = *c.pythonField(key)
	}
	return err
}

// Default returns a config with all defaults applied.
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

func exe(name string) string {
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		return name + ".exe"
	}
	return name
}

func defaultPython() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(".venv", "Scripts", "python.exe")
	}
	return filepath.Join(".venv", "bin", "python")
}

// defaultPythonOpenVINO is the interpreter of the OpenVINO environment
// (SPEC-v2 I1, I7).
func defaultPythonOpenVINO() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(".venv-ov", "Scripts", "python.exe")
	}
	return filepath.Join(".venv-ov", "bin", "python")
}

// defaultPythonMLX is the interpreter of the MLX environment (SPEC-v3 V5).
func defaultPythonMLX() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(".venv-mlx", "Scripts", "python.exe")
	}
	return filepath.Join(".venv-mlx", "bin", "python")
}

// pythonKeys are the config.toml keys of the worker interpreters.
var pythonKeys = []string{"python", "python_openvino", "python_mlx"}

// pythonField is the field of an interpreter key (PythonKey).
func (c *Config) pythonField(key string) *string {
	switch key {
	case "python_openvino":
		return &c.PythonOpenVINO
	case "python_mlx":
		return &c.PythonMLX
	}
	return &c.Python
}

// devPythonDefault is the development default of an interpreter key.
func devPythonDefault(key string) string {
	switch key {
	case "python_openvino":
		return defaultPythonOpenVINO()
	case "python_mlx":
		return defaultPythonMLX()
	}
	return defaultPython()
}

// applyPythonDefaults: a configured interpreter gets .exe on Windows; an
// empty one the development default (.venv, .venv-ov, .venv-mlx), except
// in the desktop application (ResourcesDir set), where it stays empty: the
// components provide the interpreters there (WorkerPython).
func (c *Config) applyPythonDefaults() {
	for _, key := range pythonKeys {
		p := c.pythonField(key)
		if *p != "" {
			*p = exe(*p)
			continue
		}
		if c.ResourcesDir != "" {
			continue
		}
		*p = devPythonDefault(key)
		if c.devPython == nil {
			c.devPython = map[string]string{}
		}
		c.devPython[key] = *p
	}
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8420"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	c.applyPythonDefaults()
	if c.FFmpeg == "" {
		c.FFmpeg = "ffmpeg"
	}
	c.FFmpeg = exe(c.FFmpeg)
	if c.NvidiaSMI == "" {
		c.NvidiaSMI = "nvidia-smi"
	}
	c.NvidiaSMI = exe(c.NvidiaSMI)
	if c.Aria2c == "" {
		c.Aria2c = "aria2c"
	}
	c.Aria2c = exe(c.Aria2c)
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.WorkerDir == "" {
		c.WorkerDir = "worker"
	}

	a := &c.ASR
	if a.Model == "" {
		a.Model = "large-v3"
	}
	if a.ComputeType == "" {
		a.ComputeType = "auto"
	}
	if a.Device == "" {
		a.Device = "cuda"
	}
	if a.Language == "" {
		a.Language = "en"
	}
	if a.VRAMPerJobMB == 0 {
		a.VRAMPerJobMB = DefaultVRAMPerJobMB
	}
	if a.MaxParallel == 0 {
		a.MaxParallel = 1
	}
	if a.VRAMReserveMB == 0 {
		a.VRAMReserveMB = 1000
	}
	if a.WorkerMemBudgetMB <= 0 {
		a.WorkerMemBudgetMB = DefaultWorkerMemBudgetMB
	}

	l := &c.LLM
	if l.Providers == nil {
		l.Providers = map[string]Provider{}
	}
	if len(l.Providers) == 0 {
		l.Providers["deepseek"] = Provider{}
	}
	for name, p := range l.Providers {
		if name == "deepseek" {
			if p.BaseURL == "" {
				p.BaseURL = "https://api.deepseek.com"
			}
			if p.APIKeyEnv == "" && p.APIKey == "" {
				p.APIKeyEnv = "DEEPSEEK_API_KEY"
			}
		}
		// DeepSeek limits concurrent requests per account and has no
		// per-minute limit (perf-llm, NOTES.md): 64 at a time and
		// requests_per_minute 0 = no limit. Other providers keep 8 / 300.
		deepseek := name == "deepseek" || strings.Contains(strings.ToLower(p.BaseURL), "api.deepseek.com")
		if p.MaxConcurrency == 0 {
			switch {
			case p.Local:
				p.MaxConcurrency = 1 // SPEC §17.2
			case deepseek:
				p.MaxConcurrency = DeepSeekMaxConcurrency
			default:
				p.MaxConcurrency = 8
			}
		}
		if p.RequestsPerMinute == 0 && !deepseek {
			p.RequestsPerMinute = 300
		}
		l.Providers[name] = p
	}
	if l.Roles == nil {
		l.Roles = map[string]Role{}
	}
	first := "deepseek"
	if _, ok := l.Providers[first]; !ok {
		for name := range l.Providers {
			first = name
			break
		}
	}
	for _, r := range []string{RoleGlossary, RoleSummary, RoleRepair, RoleTranslate} {
		role := l.Roles[r]
		if role.Provider == "" {
			role.Provider = first
		}
		if role.Model == "" {
			role.Model = DefaultDeepSeekModel
		}
		if r == RoleTranslate && role.Window == 0 {
			if l.Providers[role.Provider].Local {
				role.Window = 1
			} else {
				role.Window = 3
			}
		}
		l.Roles[r] = role
	}
}

func (c *Config) validate() error {
	var errs []string
	switch c.ASR.ComputeType {
	case "auto", "float16", "int8_float16":
	default:
		errs = append(errs, "asr.compute_type must be auto|float16|int8_float16")
	}
	switch c.ASR.Device {
	case "cuda", "cpu":
	default:
		errs = append(errs, "asr.device must be cuda|cpu")
	}
	for name, r := range c.LLM.Roles {
		if _, ok := c.LLM.Providers[r.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("llm.roles.%s: unknown provider %q", name, r.Provider))
		}
	}
	if !strings.HasPrefix(c.Listen, "127.0.0.1:") && !strings.HasPrefix(c.Listen, "localhost:") {
		errs = append(errs, "listen must be on 127.0.0.1 (SPEC §0)")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// ResolveComputeType applies SPEC §17.2 to a configured compute type:
// an explicit value wins; auto → int8 on device cpu, int8_float16 below
// 10 GB total VRAM, else float16. totalMB <= 0 (unknown) resolves to
// int8_float16, the safer choice. v1.1 callers pass the values of
// settings.ASR() (A5.3), which replaced the [asr] section.
func ResolveComputeType(computeType, device string, totalMB int) string {
	if computeType != "auto" && computeType != "" {
		return computeType
	}
	if device == "cpu" {
		return "int8"
	}
	if totalMB >= 10*1024 {
		return "float16"
	}
	return "int8_float16"
}

// ResolveComputeType is ResolveComputeType with the legacy [asr] fields.
func (c *Config) ResolveComputeType(totalMB int) string {
	return ResolveComputeType(c.ASR.ComputeType, c.ASR.Device, totalMB)
}

// PriceFor returns the configured price for provider/model, if any.
func (c *Config) PriceFor(provider, model string) (Price, bool) {
	m, ok := c.LLM.Prices[provider]
	if !ok {
		return Price{}, false
	}
	p, ok := m[model]
	return p, ok
}

// Redacted returns a copy safe for GET /api/system: API keys masked as sk-…last4.
func (c *Config) Redacted() *Config {
	cp := *c
	cp.Components = nil // run-time state, not configuration
	cp.LLM.Providers = map[string]Provider{}
	for name, p := range c.LLM.Providers {
		if k := p.Key(); k != "" && p.APIKey != "" {
			p.APIKey = Mask(k)
		}
		cp.LLM.Providers[name] = p
	}
	return &cp
}

// Mask formats a key as "sk-…abcd" (SPEC §9.10).
func Mask(key string) string {
	if len(key) <= 4 {
		return "sk-…"
	}
	return "sk-…" + key[len(key)-4:]
}
