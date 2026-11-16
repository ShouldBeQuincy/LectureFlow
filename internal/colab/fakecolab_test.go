// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// A stateful fake of Colab and one runtime's Jupyter server for the batch
// tests (colab-batch): the strict replays cannot express two lectures at
// once or uploads ahead. Contents live in memory; a cell that attaches to
// the resident worker behaves like worker/colab_remote.py --attach (setup
// stages once per runtime, the lecture's events once its job.json exists,
// result.json written, the audio removed after "decoding", the lecture
// dropped when its job.json disappears); a memory probe answers with the
// configured decision.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeColab struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	files       map[string][]byte // contents path (no leading /) -> data
	dirs        map[string]bool
	kernels     map[string]bool
	nKernels    int
	assigned    bool
	accelerator string
	workerReady bool
	setups      int // cells that went through the worker's setup
	// running: lectures being transcribed; maxRunning the most at once
	running, maxRunning int
	// uploads: per file, the times its chunks were written (first, last)
	uploadSpans map[string][2]time.Time
	uploadCount map[string]int
	deleted     []string
	cells       []string // the code of every cell
	// gates hold a lecture (by job directory name) in its transcription
	// until closed; transcribing gets the name when it starts.
	gates        map[string]chan struct{}
	transcribing chan string
	// memory: the probe's answer (nil: the cell prints nothing)
	memory map[string]any
	// setupErr: the worker's setup fails with this code
	setupErr string
	// failUpload: a PUT of a path with this suffix answers 500; failDelete
	// likewise for a DELETE
	failUpload, failDelete string
	released               bool
	balance                float64
	// colab-obs-core: ccu-info's eligibleGpus (nil: left out) and
	// freeCcuQuotaInfo.remainingTokens ("" : left out); ccuReads counts
	// the readings; resources is the runtime's /api/colab/resources answer
	// (nil: 404), resourceReads counts them.
	eligible      []string
	freeTokens    string
	ccuReads      int
	resources     map[string]any
	resourceReads int
	// ccuStatus / ccuRaw replace the ccu-info answer (0 / "": the one above)
	ccuStatus int
	ccuRaw    string
	// assignP is GET /tun/m/assign's "p" (reCAPTCHA wanted)
	assignP bool
	// The removal cell (3.2.0): removed lists the directories it removed;
	// removeLeft makes it answer that the directory is still there;
	// removeGate, when set, holds it until closed (removing is closed when
	// it started).
	removed    []string
	removeLeft bool
	removeGate chan struct{}
	removing   chan struct{}
	// deleteDelay slows every Contents API DELETE down (the proxy stalls
	// of accept-320)
	deleteDelay time.Duration
	// bootGate, when set, holds the first boot probe (GET /api/kernels)
	// until closed; booting is closed when that probe arrived.
	bootGate chan struct{}
	booting  chan struct{}
	// faults (v3.2.x): requests that fail the first times, hang until the
	// client gives up, or lose their connection (fault).
	faults []*fakeFault
	// unassignGate, when set, holds the release (POST unassign) until
	// closed; unassigning is closed when it arrived.
	unassignGate chan struct{}
	unassigning  chan struct{}
	// assignGate, when set, holds the next POST assign until closed (the
	// runtime is assigned when it goes on); assigning is closed when it
	// arrived.
	assignGate chan struct{}
	assigning  chan struct{}
	// probes counts GET /api/kernels (the boot probe and the reconnect probes)
	probes int
	// multi: every POST assign gets a runtime of its own (gpu-fake-1,
	// gpu-fake-2, ...), listed until its own unassign; live holds those
	// still assigned. They share the one Jupyter server; its files go only
	// when the last runtime is released.
	multi     bool
	nAssigned int
	live      []string
	// proxyTok replaces the runtime proxy token the fake hands out (nil:
	// "fake-proxy-token"); the fake's runtime itself accepts any.
	proxyTok atomic.Pointer[string]
	// result replaces the result.json the worker writes ("": one sentence);
	// declaredBytes replaces the size its done event reports (0: the real
	// one); resultGets counts the GETs of a result.json.
	result        string
	declaredBytes int64
	resultGets    int
}

// liveEndpoints returns the runtimes still assigned (multi).
func (f *fakeColab) liveEndpoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.live...)
}

// fakeFault makes matching requests fail: after skip of them went through,
// the next times (-1: every one) answer status, hang until the client gives
// up (hang) or lose their connection without an answer (drop). With apply a
// Contents API PUT is written first: the answer is lost, not the request.
type fakeFault struct {
	method string // "" = any
	suffix string // of the request path
	chunk  int    // 0 = any; else the PUT's "chunk"
	skip   int
	times  int
	status int
	hang   bool
	drop   bool
	apply  bool
	hits   int // how many requests it failed
}

// fault adds ft and returns it (faultHits tells how often it struck).
func (f *fakeColab) fault(ft fakeFault) *fakeFault {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &ft
	f.faults = append(f.faults, p)
	return p
}

func (f *fakeColab) faultHits(ft *fakeFault) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ft.hits
}

// clearFaults removes every fault: the runtime answers again.
func (f *fakeColab) clearFaults() {
	f.mu.Lock()
	f.faults = nil
	f.mu.Unlock()
}

func (f *fakeColab) wasReleased() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

// faultFor returns the fault that strikes this request, if any.
func (f *fakeColab) faultFor(r *http.Request, body []byte) *fakeFault {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ft := range f.faults {
		if ft.method != "" && ft.method != r.Method || !strings.HasSuffix(r.URL.Path, ft.suffix) {
			continue
		}
		if ft.chunk != 0 {
			var m struct {
				Chunk int `json:"chunk"`
			}
			if json.Unmarshal(body, &m) != nil || m.Chunk != ft.chunk {
				continue
			}
		}
		if ft.skip > 0 {
			ft.skip--
			continue
		}
		if ft.times == 0 {
			continue
		}
		if ft.times > 0 {
			ft.times--
		}
		ft.hits++
		return ft
	}
	return nil
}

// lostAnswer is a ResponseWriter whose answer never reaches the client.
type lostAnswer struct{ h http.Header }

func (d lostAnswer) Header() http.Header       { return d.h }
func (lostAnswer) Write(b []byte) (int, error) { return len(b), nil }
func (lostAnswer) WriteHeader(int)             {}

func newFakeColab(t *testing.T) *fakeColab {
	f := &fakeColab{t: t, files: map[string][]byte{}, dirs: map[string]bool{}, kernels: map[string]bool{},
		accelerator: "T4", uploadSpans: map[string][2]time.Time{}, uploadCount: map[string]int{},
		gates: map[string]chan struct{}{}, transcribing: make(chan string, 16), balance: 95.5,
		eligible: []string{"G4", "A100", "L4", "T4"}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		for k, g := range f.gates {
			select {
			case <-g:
			default:
				close(g)
			}
			delete(f.gates, k)
		}
		f.mu.Unlock()
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

func (f *fakeColab) options(o *Options) {
	o.ColabURL, o.TokenURL, o.RevokeURL = f.srv.URL+"/colab", f.srv.URL+"/oauth/token", f.srv.URL+"/oauth/revoke"
}

// hold makes the lecture of job directory name wait in its transcription
// until release(name); name "*" holds every lecture without a gate of its
// own.
func (f *fakeColab) hold(name string) {
	f.mu.Lock()
	f.gates[name] = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeColab) release(name string) {
	f.mu.Lock()
	g := f.gates[name]
	f.mu.Unlock()
	if g != nil {
		close(g)
	}
}

func (f *fakeColab) has(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[path]
	return ok
}

func (f *fakeColab) hasDir(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[path]
}

func (f *fakeColab) writeJSON(w http.ResponseWriter, status int, xssi bool, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if xssi {
		_, _ = io.WriteString(w, xssiPrefix)
	}
	_, _ = w.Write(b)
}

func (f *fakeColab) proxy() map[string]any {
	tok := "fake-proxy-token"
	if p := f.proxyTok.Load(); p != nil {
		tok = *p
	}
	return map[string]any{"token": tok, "tokenExpiresInSeconds": 3600, "url": f.srv.URL + "/rt"}
}

func (f *fakeColab) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	body, _ := io.ReadAll(r.Body)
	if ft := f.faultFor(r, body); ft != nil {
		if ft.apply && strings.HasPrefix(p, "/rt/api/contents/") {
			f.serveContents(lostAnswer{h: http.Header{}}, r, strings.TrimPrefix(p, "/rt/api/contents/"), body)
		}
		switch {
		case ft.hang:
			<-r.Context().Done() // until the client gives up (or the test ends)
		case ft.drop:
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
		default:
			http.Error(w, "fake fault", ft.status)
		}
		return
	}
	switch {
	case p == "/oauth/token":
		f.writeJSON(w, 200, false, map[string]any{"access_token": "ya29.fake", "expires_in": 3599,
			"scope": scopeOpenID + " " + scopeEmail + " " + scopeColab, "token_type": "Bearer"})
	case p == "/colab/tun/m/ccu-info":
		f.mu.Lock()
		n := 0
		if f.assigned {
			n = 1
		}
		ans := map[string]any{"currentBalance": f.balance, "consumptionRateHourly": 0, "assignmentsCount": n}
		if f.eligible != nil {
			ans["eligibleGpus"] = f.eligible
			ans["ineligibleGpus"] = []string{"H100"}
		}
		if f.freeTokens != "" {
			ans["freeCcuQuotaInfo"] = map[string]any{"remainingTokens": f.freeTokens, "nextRefillTimestampSec": 1793300000}
		}
		f.ccuReads++
		status, raw := f.ccuStatus, f.ccuRaw
		f.mu.Unlock()
		switch {
		case status != 0:
			http.Error(w, "fake", status)
		case raw != "":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, xssiPrefix+raw)
		default:
			f.writeJSON(w, 200, true, ans)
		}
	case p == "/colab/tun/m/assign" && r.Method == http.MethodGet:
		f.mu.Lock()
		p := f.assignP
		f.mu.Unlock()
		f.writeJSON(w, 200, true, map[string]any{"acc": r.URL.Query().Get("accelerator"), "nbh": "nbh", "token": "xsrf-fake", "variant": "GPU", "p": p})
	case p == "/colab/tun/m/assign" && r.Method == http.MethodPost:
		f.mu.Lock()
		gate, arrived := f.assignGate, f.assigning
		f.assignGate, f.assigning = nil, nil
		f.mu.Unlock()
		if gate != nil {
			close(arrived)
			<-gate
		}
		f.mu.Lock()
		f.assigned, f.released = true, false
		f.accelerator = r.URL.Query().Get("accelerator")
		acc, ep := f.accelerator, "gpu-fake-1"
		if f.multi {
			f.nAssigned++
			ep = fmt.Sprintf("gpu-fake-%d", f.nAssigned)
			f.live = append(f.live, ep)
		}
		f.mu.Unlock()
		f.writeJSON(w, 200, true, map[string]any{"accelerator": acc, "endpoint": ep, "runtimeProxyInfo": f.proxy(), "variant": 1})
	case p == "/colab/tun/m/assignments":
		f.mu.Lock()
		var list []any
		if f.multi {
			for _, ep := range f.live {
				list = append(list, map[string]any{"accelerator": f.accelerator, "endpoint": ep, "variant": 1, "machineShape": 0,
					"runtimeProxyInfo": f.proxy()})
			}
		} else if f.assigned {
			list = append(list, map[string]any{"accelerator": f.accelerator, "endpoint": "gpu-fake-1", "variant": 1, "machineShape": 0,
				"runtimeProxyInfo": f.proxy()})
		}
		f.mu.Unlock()
		f.writeJSON(w, 200, true, map[string]any{"assignments": list})
	case strings.HasPrefix(p, "/colab/tun/m/unassign/"):
		if r.Method == http.MethodPost {
			f.mu.Lock()
			gate, arrived := f.unassignGate, f.unassigning
			f.unassigning = nil
			f.mu.Unlock()
			if gate != nil {
				if arrived != nil {
					close(arrived)
				}
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			f.mu.Lock()
			if f.multi {
				ep := strings.TrimPrefix(p, "/colab/tun/m/unassign/")
				f.live = slices.DeleteFunc(f.live, func(x string) bool { return x == ep })
				f.released = true
				if len(f.live) > 0 {
					f.mu.Unlock()
					f.writeJSON(w, 200, true, map[string]any{})
					return
				}
			}
			f.assigned, f.released, f.workerReady = false, true, false
			f.files, f.dirs = map[string][]byte{}, map[string]bool{}
			f.mu.Unlock()
			f.writeJSON(w, 200, true, map[string]any{})
		} else {
			f.writeJSON(w, 200, true, map[string]any{"token": "xsrf-unassign-fake"})
		}
	case p == "/rt/api/kernels" && r.Method == http.MethodGet:
		f.mu.Lock()
		gate, booting := f.bootGate, f.booting
		f.bootGate = nil
		f.probes++
		f.mu.Unlock()
		if gate != nil {
			close(booting)
			<-gate
		}
		f.writeJSON(w, 200, false, []any{})
	case p == "/rt/api/kernels" && r.Method == http.MethodPost:
		f.mu.Lock()
		f.nKernels++
		id := fmt.Sprintf("kernel-%d", f.nKernels)
		f.kernels[id] = true
		f.mu.Unlock()
		f.writeJSON(w, 201, false, map[string]any{"id": id, "name": "python3"})
	case strings.HasPrefix(p, "/rt/api/kernels/") && strings.HasSuffix(p, "/channels"):
		f.serveWS(w, r)
	case strings.HasPrefix(p, "/rt/api/kernels/") && r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.kernels, strings.TrimPrefix(p, "/rt/api/kernels/"))
		f.mu.Unlock()
		w.WriteHeader(204)
	case p == "/rt/api/colab/resources":
		f.mu.Lock()
		res := f.resources
		f.resourceReads++
		f.mu.Unlock()
		if res == nil {
			http.Error(w, "no", 404)
			return
		}
		f.writeJSON(w, 200, true, res) // the real runtime prefixes the XSSI guard (colab-obs-core run, 2026-10-27)
	case strings.HasPrefix(p, "/rt/api/contents/"):
		f.serveContents(w, r, strings.TrimPrefix(p, "/rt/api/contents/"), body)
	default:
		http.Error(w, "fake: unknown "+r.Method+" "+p, 404)
	}
}

func (f *fakeColab) serveContents(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if m["type"] == "directory" {
			f.dirs[path] = true
			w.WriteHeader(201)
			return
		}
		if f.failUpload != "" && strings.HasSuffix(path, f.failUpload) {
			http.Error(w, "fake failure", 500)
			return
		}
		data, _ := base64.StdEncoding.DecodeString(m["content"].(string))
		chunk := int(m["chunk"].(float64))
		now := time.Now()
		span := f.uploadSpans[path]
		if chunk == 1 {
			f.files[path] = data
			span[0] = now
			f.uploadCount[path]++
		} else {
			f.files[path] = append(f.files[path], data...)
		}
		span[1] = now
		f.uploadSpans[path] = span
		w.WriteHeader(201)
	case http.MethodGet:
		if strings.HasSuffix(path, "/result.json") {
			f.resultGets++
		}
		data, ok := f.files[path]
		if !ok {
			http.Error(w, "no", 404)
			return
		}
		b, _ := json.Marshal(map[string]any{"type": "file", "format": "base64", "content": base64.StdEncoding.EncodeToString(data)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	case http.MethodDelete:
		if d := f.deleteDelay; d > 0 {
			f.mu.Unlock()
			time.Sleep(d)
			f.mu.Lock()
		}
		if f.failDelete != "" && strings.HasSuffix(path, f.failDelete) {
			http.Error(w, "fake failure", 500)
			return
		}
		f.deleted = append(f.deleted, path)
		delete(f.files, path)
		if f.dirs[path] {
			for k := range f.files {
				if strings.HasPrefix(k, path+"/") {
					http.Error(w, "not empty", 400)
					return
				}
			}
			delete(f.dirs, path)
		}
		w.WriteHeader(204)
	}
}

var (
	cellJobRe    = regexp.MustCompile(`_j = "/([^"]+)"`)
	cellRemoveRe = regexp.MustCompile(`_lf_dir = "/([^"]+)"`)
)

func (f *fakeColab) serveWS(w http.ResponseWriter, r *http.Request) {
	hj, _ := w.(http.Hijacker)
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		acceptKey(r.Header.Get("Sec-WebSocket-Key")))
	if brw.Flush() != nil {
		conn.Close()
		return
	}
	c := newWSConn(conn, brw.Reader, false)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, data, err := c.ReadMessage(ctx)
	if err != nil {
		return
	}
	m, err := decodeKernelMsg(false, data)
	if err != nil {
		return
	}
	var content map[string]any
	_ = json.Unmarshal(m.Content, &content)
	code, _ := content["code"].(string)
	parent, _ := m.Header["msg_id"].(string)
	session, _ := m.Header["session"].(string)
	send := func(channel, typ string, c2 map[string]any) bool {
		cb, _ := json.Marshal(c2)
		msg, _ := json.Marshal(kernelMsg{
			Header:       map[string]any{"msg_id": newUUID(), "msg_type": typ, "session": session},
			ParentHeader: map[string]any{"msg_id": parent}, Metadata: map[string]any{}, Content: cb, Channel: channel, Buffers: []any{},
		})
		return c.WriteText(ctx, msg) == nil
	}
	ts := float64(time.Now().Unix())
	lines := func(evs ...map[string]any) bool {
		var sb strings.Builder
		for _, e := range evs {
			e["ts"] = ts
			b, _ := json.Marshal(e)
			sb.Write(b)
			sb.WriteByte('\n')
		}
		return send("iopub", "stream", map[string]any{"name": "stdout", "text": sb.String()})
	}
	end := func(status string) {
		send("iopub", "status", map[string]any{"execution_state": "idle"})
		send("shell", "execute_reply", map[string]any{"status": status})
		for { // until the client hangs up
			if _, _, err := c.ReadMessage(ctx); err != nil {
				return
			}
		}
	}
	f.mu.Lock()
	f.cells = append(f.cells, code)
	f.mu.Unlock()
	send("iopub", "status", map[string]any{"execution_state": "busy"})
	if strings.Contains(code, "--memory-probe") {
		f.mu.Lock()
		mem := f.memory
		f.mu.Unlock()
		if mem != nil {
			e := map[string]any{"type": "memory"}
			for k, v := range mem {
				e[k] = v
			}
			lines(e)
		}
		end("ok")
		return
	}
	if strings.Contains(code, removedMark) {
		dm := cellRemoveRe.FindStringSubmatch(code)
		f.mu.Lock()
		gate, started, left := f.removeGate, f.removing, f.removeLeft
		f.mu.Unlock()
		if started != nil {
			close(started)
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return
			}
		}
		mark := leftMark
		if dm != nil && !left {
			dir := dm[1]
			f.mu.Lock()
			for k := range f.files {
				if strings.HasPrefix(k, dir+"/") {
					delete(f.files, k)
				}
			}
			delete(f.dirs, dir)
			f.removed = append(f.removed, dir)
			f.mu.Unlock()
			mark = removedMark
		}
		send("iopub", "stream", map[string]any{"name": "stdout", "text": mark + "\n"})
		end("ok")
		return
	}
	jm := cellJobRe.FindStringSubmatch(code)
	if jm == nil {
		end("error")
		return
	}
	jobDir := jm[1]
	name := filepath.Base(jobDir)
	f.mu.Lock()
	setup := !f.workerReady
	f.workerReady = true
	if setup {
		f.setups++
	}
	setupErr := f.setupErr
	f.mu.Unlock()
	if setup {
		lines(map[string]any{"type": "stage", "stage": "install"}, map[string]any{"type": "stage", "stage": "load_model"})
	}
	if setupErr != "" {
		lines(map[string]any{"type": "error", "code": setupErr, "retryable": true})
		end("error")
		return
	}
	// the lecture's turn: its job.json (or the directory gone: cancelled)
	for {
		f.mu.Lock()
		_, job := f.files[jobDir+"/job.json"]
		dir := f.dirs[jobDir]
		f.mu.Unlock()
		if job {
			break
		}
		if !dir {
			end("error")
			return
		}
		if ctx.Err() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	var job remoteJob
	_ = json.Unmarshal(f.files[jobDir+"/job.json"], &job)
	audio, ok := f.files[jobDir+"/"+job.Audio]
	f.running++
	f.maxRunning = max(f.maxRunning, f.running)
	gate := f.gates[name]
	if gate == nil {
		gate = f.gates["*"] // hold("*"): every lecture, whatever its directory name
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if !ok || sha256Hex(audio) != job.AudioSHA256 {
		lines(map[string]any{"type": "stage", "stage": "check"}, map[string]any{"type": "error", "code": "audio_hash_mismatch", "retryable": true})
		end("error")
		return
	}
	f.mu.Lock()
	delete(f.files, jobDir+"/"+job.Audio) // decoded, then removed
	f.mu.Unlock()
	lines(map[string]any{"type": "stage", "stage": "check"}, map[string]any{"type": "stage", "stage": "transcribe"},
		map[string]any{"type": "progress", "fraction": 0.5})
	f.transcribing <- name
	if gate != nil {
		for {
			select {
			case <-gate:
			case <-time.After(5 * time.Millisecond):
				f.mu.Lock()
				_, still := f.files[jobDir+"/job.json"]
				f.mu.Unlock()
				if !still { // cancelled: the worker drops the lecture
					end("error")
					return
				}
				continue
			}
			break
		}
	}
	result := `{"prompt_terms_used":0,"sentences":[{"start_ms":0,"end_ms":500,"text":"x","low_conf_words":[],"words":[]}]}`
	f.mu.Lock()
	if f.result != "" {
		result = f.result
	}
	size := int64(len(result))
	if f.declaredBytes > 0 {
		size = f.declaredBytes
	}
	f.files[jobDir+"/result.json"] = []byte(result)
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(result))
	lines(map[string]any{"type": "progress", "fraction": 1.0}, map[string]any{"type": "stage", "stage": "write"},
		map[string]any{"type": "done", "result": "result.json", "sha256": hex.EncodeToString(sum[:]), "bytes": size,
			"audio_s": 600.0, "load_s": 0.0, "transcribe_s": 60.0, "sentences": 1})
	end("ok")
}

// fakeEnv is a Backend on the fake with a credential and a fake clock.
type fakeEnv struct {
	b   *Backend
	f   *fakeColab
	kv  *MemKV
	clk *fakeClock
	log *logBuf
	dir string
}

func newFakeEnv(t *testing.T, set Settings, mods ...func(*Options)) *fakeEnv {
	t.Helper()
	f := newFakeColab(t)
	kv := newDiskKV()
	raw, _ := json.Marshal(set)
	kv.m[KVSettings] = string(raw)
	clk := newFakeClock()
	lg, lb := newTestLogger()
	o := Options{KV: kv, WorkerDir: workerDir(t), FFmpeg: "ffmpeg", RunFFmpeg: fakeFFmpeg(300), Logger: lg, Now: clk.Now,
		Sleep: clk.Sleep, RunID: "run-1", AllowHTTP: true, ChunkSize: 100, ADCPath: filepath.Join(t.TempDir(), "absent.json"),
		Retry: RetryOptions{Jitter: -1}} // exact waits; the jitter has its own test
	f.options(&o)
	for _, m := range mods {
		m(&o)
	}
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "run-1")
	if err := b.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.bgCancel() })
	return &fakeEnv{b: b, f: f, kv: kv, clk: clk, log: lb, dir: t.TempDir()}
}

// lecture writes a lecture's audio (distinct bytes per name) and its Job.
func (e *fakeEnv) lecture(t *testing.T, name string) Job {
	t.Helper()
	d := filepath.Join(e.dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	wav := filepath.Join(d, "audio.wav")
	if err := os.WriteFile(wav, []byte("RIFF....WAVEfmt "+name), 0o644); err != nil {
		t.Fatal(err)
	}
	return Job{ID: name, AudioPath: wav, DurationSec: 600, Language: "en", OutDir: d}
}

func (e *fakeEnv) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *fakeEnv) transcribingNow(t *testing.T) string {
	t.Helper()
	select {
	case n := <-e.f.transcribing:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no lecture started transcribing")
		return ""
	}
}
