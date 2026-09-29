package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// fakeDaemon es un daemon de kindling en memoria con un kling-phoned falso
// dentro de cada máquina: lo justo para ejercitar la extensión sin microVMs.
// Hace cumplir lo que importa: la API del teléfono cerrada sin token, el
// documento de MMDS que aplica el gancho y la marca de secretos.
type fakeDaemon struct {
	t  *testing.T
	mu sync.Mutex

	seq      int
	machines map[string]*fakeMachine // por id
	snaps    map[string]*api.Snapshot
	store    map[string]json.RawMessage // ns/key
	commits  []string
	// verifyBad hace que verify-cache diga que la caché no casa.
	verifyBad bool
	// guestCalls cuenta llamadas al proxy por ruta.
	guestCalls map[string]int
}

type fakeMachine struct {
	m      *api.Machine
	mmds   json.RawMessage
	phone  *fakePhone
	secret bool
}

// fakePhone es el estado de kling-phoned en esa máquina.
type fakePhone struct {
	androidID, serial, name string
	adbKeys                 []string
	tokens                  []apiToken
	taps                    [][2]int
	keys                    []string
	texts                   []string
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	return &fakeDaemon{t: t, machines: map[string]*fakeMachine{}, snaps: map[string]*api.Snapshot{},
		store: map[string]json.RawMessage{}, guestCalls: map[string]int{}}
}

func notFound(what string) error { return &api.StatusError{Code: 404, Message: what + " not found"} }

func (f *fakeDaemon) find(ref string) *fakeMachine {
	if fm := f.machines[ref]; fm != nil {
		return fm
	}
	for _, fm := range f.machines {
		if fm.m.Name == ref {
			return fm
		}
	}
	return nil
}

func cp(m *api.Machine) *api.Machine {
	c := *m
	c.Labels = api.MergeLabels(m.Labels, nil)
	return &c
}

func (f *fakeDaemon) Info(context.Context) (*api.Info, error) {
	return &api.Info{Version: "test", Arch: "arm64"}, nil
}

func (f *fakeDaemon) List(context.Context) ([]*api.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*api.Machine
	for _, fm := range f.machines {
		out = append(out, cp(fm.m))
	}
	return out, nil
}

func (f *fakeDaemon) Get(_ context.Context, ref string) (*api.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return nil, notFound("machine")
	}
	return cp(fm.m), nil
}

func (f *fakeDaemon) Run(_ context.Context, r api.RunRequest) (*api.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.find(r.Name) != nil {
		return nil, &api.StatusError{Code: 409, Message: "name in use"}
	}
	f.seq++
	id := fmt.Sprintf("%016x", 0xabc000+f.seq)
	m := &api.Machine{ID: id, Name: r.Name, State: api.StateRunning, Image: r.Image, From: r.From, Egress: r.Egress,
		VCPUs: r.VCPUs, MemMiB: r.MemMiB, CreatedAt: time.Now(),
		Forwards: map[string]string{"5555": fmt.Sprintf("127.0.0.1:%d", 40000+f.seq), "8091": fmt.Sprintf("127.0.0.1:%d", 41000+f.seq)}}
	fm := &fakeMachine{m: m, phone: &fakePhone{}}
	if r.From != "" {
		s := f.snaps[r.From]
		if s == nil {
			return nil, notFound("snapshot")
		}
		m.Labels = api.MergeLabels(s.Labels, r.Labels)
		s.Instances++
	} else {
		m.Labels = api.MergeLabels(nil, r.Labels)
	}
	f.machines[id] = fm
	return cp(m), nil
}

func (f *fakeDaemon) Remove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return notFound("machine")
	}
	if s := f.snaps[fm.m.From]; s != nil {
		s.Instances--
	}
	delete(f.machines, fm.m.ID)
	return nil
}

func (f *fakeDaemon) setState(ref string, st api.State) (*api.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return nil, notFound("machine")
	}
	if st == api.StateWarm && fm.secret {
		return nil, &api.StatusError{Code: 409, Message: "has secrets"}
	}
	fm.m.State = st
	return cp(fm.m), nil
}

func (f *fakeDaemon) Pause(_ context.Context, ref string) (*api.Machine, error) {
	return f.setState(ref, api.StatePaused)
}
func (f *fakeDaemon) Thaw(_ context.Context, ref string) (*api.Machine, error) {
	return f.setState(ref, api.StateRunning)
}
func (f *fakeDaemon) Freeze(_ context.Context, ref string) (*api.Machine, error) {
	return f.setState(ref, api.StateWarm)
}

func (f *fakeDaemon) PutMMDS(_ context.Context, ref string, data any) (*api.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return nil, notFound("machine")
	}
	b, _ := json.Marshal(data)
	fm.mmds = b
	if string(b) == "{}" {
		// El gancho ya consumió lo anterior: el daemon levanta la marca.
		fm.secret = false
	} else {
		fm.secret = true
	}
	fm.m.HasSecrets = fm.secret
	return cp(fm.m), nil
}

// RunHooks hace lo que kling-phoned identity: aplica el documento de MMDS.
func (f *fakeDaemon) RunHooks(_ context.Context, ref string, _ time.Duration) (*api.ReadyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return nil, notFound("machine")
	}
	var d struct {
		Phone *struct {
			AndroidID string      `json:"android_id"`
			Name      string      `json:"name"`
			Serial    string      `json:"serial"`
			AdbKeys   []string    `json:"adb_keys"`
			APITokens *[]apiToken `json:"api_tokens"`
		} `json:"phone"`
	}
	_ = json.Unmarshal(fm.mmds, &d)
	if d.Phone != nil {
		// Nunca un token en claro: solo sha256 de 64 hex.
		if d.Phone.APITokens != nil {
			for _, t := range *d.Phone.APITokens {
				if len(t.SHA256) != 64 {
					return &api.ReadyResult{Ready: api.ReadyFailed, Guest: &api.GuestReady{Detail: "bad token"}}, nil
				}
			}
			fm.phone.tokens = append([]apiToken(nil), *d.Phone.APITokens...)
		}
		if d.Phone.AndroidID != "" {
			p := fm.phone
			p.androidID, p.serial, p.name, p.adbKeys = d.Phone.AndroidID, d.Phone.Serial, d.Phone.Name, d.Phone.AdbKeys
		}
	}
	return &api.ReadyResult{Ready: api.ReadyYes}, nil
}

func (f *fakeDaemon) authorized(p *fakePhone, hdr map[string]string, scope string) bool {
	tok := strings.TrimPrefix(hdr["Authorization"], "Bearer ")
	if tok == "" {
		return false
	}
	s := sha256.Sum256([]byte(tok))
	h := hex.EncodeToString(s[:])
	for _, t := range p.tokens {
		if t.SHA256 == h && (t.Scope == "control" || scope == "read") {
			return true
		}
	}
	return false
}

var fakePNG = []byte("\x89PNG\r\n\x1a\nfake-screen")

// Guest es el proxy del daemon hacia el kling-phoned falso.
func (f *fakeDaemon) Guest(_ context.Context, ref string, r api.GuestRequest) (*api.GuestResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return nil, notFound("machine")
	}
	if fm.m.State != api.StateRunning {
		return nil, &api.StatusError{Code: 409, Message: "the machine is " + string(fm.m.State)}
	}
	if r.Port != phonedPort {
		return nil, &api.StatusError{Code: 403, Message: "port"}
	}
	u, _ := url.Parse(r.Path)
	f.guestCalls[r.Method+" "+u.Path]++
	p := fm.phone
	js := func(code int, v any) (*api.GuestResponse, error) {
		b, _ := json.Marshal(v)
		return &api.GuestResponse{Status: code, Body: string(b), Headers: map[string]string{"Content-Type": "application/json"}}, nil
	}
	if r.Method == "GET" && u.Path == "/v1/health" {
		return js(200, map[string]any{"ok": true, "state": "running", "boot_completed": true, "system_server": true,
			"verity": "none", "kernel": "6.1.0-android", "version": "test", "net": "veth", "api_tokens": len(p.tokens)})
	}
	scope := "control"
	if r.Method == "GET" && (u.Path == "/v1/screen" || u.Path == "/v1/tree") {
		scope = "read"
	}
	if !f.authorized(p, r.Headers, scope) {
		return js(401, map[string]string{"error": "missing or invalid API token"})
	}
	body := []byte(r.Body)
	if u.Query().Get("encoding") == "base64" && r.Method == "POST" {
		var err error
		if body, err = base64.StdEncoding.DecodeString(r.Body); err != nil {
			return js(400, map[string]string{"error": "bad base64"})
		}
	}
	switch r.Method + " " + u.Path {
	case "GET /v1/screen":
		if u.Query().Get("encoding") != "base64" {
			return js(500, map[string]string{"error": "binary over the proxy"})
		}
		return &api.GuestResponse{Status: 200, Body: base64.StdEncoding.EncodeToString(fakePNG),
			Headers: map[string]string{"Content-Type": "text/plain", "X-Phoned-Content-Type": "image/png"}}, nil
	case "GET /v1/tree":
		return &api.GuestResponse{Status: 200, Body: `<hierarchy><node text="` + fm.m.Name + `"/></hierarchy>`,
			Headers: map[string]string{"X-Phoned-Source": "uidump"}}, nil
	case "POST /v1/tap":
		var in struct{ X, Y int }
		_ = json.Unmarshal(body, &in)
		p.taps = append(p.taps, [2]int{in.X, in.Y})
		return js(200, map[string]any{"ok": true, "via": "uidump"})
	case "POST /v1/swipe", "POST /v1/launch":
		return js(200, map[string]any{"ok": true})
	case "POST /v1/key":
		var in struct{ Key string }
		_ = json.Unmarshal(body, &in)
		p.keys = append(p.keys, in.Key)
		return js(200, map[string]any{"ok": true})
	case "POST /v1/text":
		var in struct{ Text string }
		_ = json.Unmarshal(body, &in)
		p.texts = append(p.texts, in.Text)
		return js(200, map[string]any{"ok": true})
	case "POST /v1/install":
		if !bytes.HasPrefix(body, []byte("PK\x03\x04")) {
			return js(400, map[string]string{"error": "not an APK"})
		}
		return js(200, map[string]any{"ok": true, "output": "Success", "bytes": len(body)})
	case "GET /v1/identity":
		return js(200, map[string]any{"serial": p.serial, "android_id": p.androidID, "device_name": p.name, "adb_keys": len(p.adbKeys)})
	case "POST /v1/verify-cache":
		if f.verifyBad {
			return js(409, map[string]any{"ok": false, "files": 10, "mismatches": []string{"/system/lib64/libc.so"}})
		}
		return js(200, map[string]any{"ok": true, "files": 1234, "bytes": 5 << 20, "seconds": 1.5})
	case "GET /v1/logs":
		return &api.GuestResponse{Status: 200, Body: "--------- beginning of crash\n"}, nil
	}
	return js(404, map[string]string{"error": "no route"})
}

func (f *fakeDaemon) Commit(_ context.Context, ref, name string, replace bool) (*api.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return nil, notFound("machine")
	}
	if fm.secret {
		return nil, &api.StatusError{Code: 409, Message: "has secrets"}
	}
	if len(fm.phone.tokens) != 0 {
		f.t.Errorf("golden saved with %d API tokens in its RAM: it must have none", len(fm.phone.tokens))
	}
	if _, ok := f.snaps[name]; ok && !replace {
		return nil, &api.StatusError{Code: 409, Message: "exists"}
	}
	s := &api.Snapshot{Name: name, Image: fm.m.Image, Egress: fm.m.Egress, Labels: api.MergeLabels(fm.m.Labels, nil),
		Annotations: map[string]json.RawMessage{}}
	f.snaps[name] = s
	f.commits = append(f.commits, name)
	return s, nil
}

func (f *fakeDaemon) addGolden(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[name] = &api.Snapshot{Name: name, Egress: "none", Annotations: map[string]json.RawMessage{},
		Labels: map[string]string{labelPhone: "1", labelGolden: name, api.LabelPorts: "5555,8091"}}
}

func (f *fakeDaemon) Snapshot(_ context.Context, name string) (*api.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.snaps[name]
	if s == nil {
		return nil, notFound("snapshot")
	}
	c := *s
	return &c, nil
}

func (f *fakeDaemon) SetAnnotation(_ context.Context, name, key string, v any) (*api.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.snaps[name]
	if s == nil {
		return nil, notFound("snapshot")
	}
	b, _ := json.Marshal(v)
	s.Annotations[key] = b
	return s, nil
}

func (f *fakeDaemon) RemoveSnapshot(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.snaps, name)
	return nil
}

func (f *fakeDaemon) GetStore(_ context.Context, ns, key string, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.store[ns+"/"+key]
	if !ok {
		return notFound("key")
	}
	return json.Unmarshal(b, out)
}

func (f *fakeDaemon) PutStore(_ context.Context, ns, key string, v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(v)
	f.store[ns+"/"+key] = b
	return nil
}

func (f *fakeDaemon) DeleteStore(_ context.Context, ns, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.store[ns+"/"+key]; !ok {
		return notFound("key")
	}
	delete(f.store, ns+"/"+key)
	return nil
}

func (f *fakeDaemon) StoreKeys(_ context.Context, ns string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.store {
		if strings.HasPrefix(k, ns+"/") {
			out = append(out, strings.TrimPrefix(k, ns+"/"))
		}
	}
	return out, nil
}

func (f *fakeDaemon) SetLabels(_ context.Context, ref string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.find(ref)
	if fm == nil {
		return notFound("machine")
	}
	fm.m.Labels = api.MergeLabels(fm.m.Labels, labels)
	return nil
}

func (f *fakeDaemon) ImageRecipe(_ context.Context, name string) (*api.ImageRecipe, error) {
	return &api.ImageRecipe{Name: name, Spec: json.RawMessage(`{"arch":"arm64","verity_root_hash":"2e6aaddcbc740c3a9678f1f4d8e2e7f3"}`)}, nil
}

func (f *fakeDaemon) Logs(context.Context, string, int) (string, error) { return "", nil }

// phoneOf es el estado del kling-phoned falso de la máquina name.
func (f *fakeDaemon) phoneOf(name string) *fakePhone {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fm := f.find(name); fm != nil {
		return fm.phone
	}
	return nil
}

func (f *fakeDaemon) machine(name string) *fakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(name)
}

// testApp es una app contra el daemon falso, sin esperas de verdad.
func testApp(t *testing.T) (*app, *fakeDaemon, *bytes.Buffer) {
	f := newFakeDaemon(t)
	var out bytes.Buffer
	s := defaultSettings()
	s.AdbPubKey = "/nonexistent"
	a := &app{d: f, s: s, out: &out, errw: io.Discard, now: time.Now, sleep: func(time.Duration) {},
		namesMu: &sync.Mutex{}}
	return a, f, &out
}
