package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

const asExtension = "KLING_PHONE_AS_EXTENSION"

func TestMain(m *testing.M) {
	if os.Getenv(asExtension) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestManifest(t *testing.T) {
	cmd := exec.Command(os.Args[0], "--kling-manifest")
	cmd.Env = append(os.Environ(), asExtension+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--kling-manifest: %v", err)
	}
	var m plugin.Manifest
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("invalid manifest: %v", err)
	}
	if want := manifest(); !reflect.DeepEqual(m, want) {
		t.Fatalf("printed manifest differs:\n got %+v\nwant %+v", m, want)
	}
	for _, c := range m.Commands {
		if c.Usage == "" {
			t.Errorf("command %s has no usage", c.Name)
		}
	}
}

// Cada clon sale con su identidad y su token; el token no pasa por MMDS (solo
// su sha256), MMDS queda vacío y la marca de secretos levantada.
func TestUpIdentityAndToken(t *testing.T) {
	a, f, out := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	if err := a.up(ctx, 2, "phone-golden"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "phone-1\tadb 127.0.0.1:") || !strings.Contains(out.String(), "phone-2\t") {
		t.Fatalf("output:\n%s", out)
	}
	ids := map[string]bool{}
	serials := map[string]bool{}
	for _, n := range []string{"phone-1", "phone-2"} {
		fm := f.machine(n)
		if fm == nil {
			t.Fatalf("%s not created", n)
		}
		p := fm.phone
		if len(p.androidID) != 16 || p.serial == "" || p.name != n {
			t.Fatalf("%s identity: %+v", n, p)
		}
		ids[p.androidID], serials[p.serial] = true, true
		if string(fm.mmds) != "{}" || fm.m.HasSecrets {
			t.Fatalf("%s: MMDS not emptied (%s) or still marked", n, fm.mmds)
		}
		var rec tokenRec
		if err := f.GetStore(ctx, storeNS, fm.m.ID, &rec); err != nil {
			t.Fatalf("%s: no token in the store: %v", n, err)
		}
		if len(p.tokens) != 1 || p.tokens[0].SHA256 != tokenHash(rec.Control) || p.tokens[0].Scope != "control" {
			t.Fatalf("%s: phone tokens %+v do not match the stored one", n, p.tokens)
		}
		if fm.m.Labels[labelPhone] != "1" || fm.m.Labels[labelGolden] != "phone-golden" {
			t.Fatalf("%s labels: %v", n, fm.m.Labels)
		}
	}
	if len(ids) != 2 || len(serials) != 2 {
		t.Fatalf("clones share identity: %v %v", ids, serials)
	}
}

// Sin token (lo que tendría una arista de grafo) la API del teléfono no deja
// hacer nada; con el del store, sí.
func TestAPINeedsToken(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	if err := a.up(ctx, 1, "phone-golden"); err != nil {
		t.Fatal(err)
	}
	m, err := a.phone(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.call(ctx, m, "", "POST", "/v1/tap", []byte(`{"x":1,"y":2}`), false)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("tap without token: %v", err)
	}
	if _, err := a.callPhone(ctx, m, "POST", "/v1/tap", []byte(`{"x":1,"y":2}`), false); err != nil {
		t.Fatal(err)
	}
	r, err := a.callPhone(ctx, m, "GET", "/v1/screen", nil, false)
	if err != nil || !bytes.Equal(r.Body, fakePNG) || r.ContentType != "image/png" {
		t.Fatalf("screen: %v %q", err, r)
	}
	// Un token de lectura acuñado ve la pantalla y no toca.
	ro, err := a.tokenCmd(ctx, m, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.call(ctx, m, ro, "GET", "/v1/tree", nil, false); err != nil {
		t.Fatalf("read token on tree: %v", err)
	}
	if _, err := a.call(ctx, m, ro, "POST", "/v1/tap", []byte(`{"x":1,"y":2}`), false); err == nil {
		t.Fatal("read token could tap")
	}
	// Rotar: el viejo deja de abrir, el nuevo abre, la identidad no cambia.
	old, _ := a.token(ctx, m)
	before := *f.phoneOf("phone-1")
	nt, err := a.tokenCmd(ctx, m, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.call(ctx, m, old.Control, "GET", "/v1/screen", nil, false); err == nil {
		t.Fatal("the old control token still works after rotating")
	}
	if _, err := a.call(ctx, m, nt, "GET", "/v1/screen", nil, false); err != nil {
		t.Fatalf("new token: %v", err)
	}
	if after := f.phoneOf("phone-1"); after.androidID != before.androidID || after.serial != before.serial {
		t.Fatal("rotating the token changed the identity")
	}
	// rm quita la máquina y su token.
	if err := a.rm(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store[storeNS+"/"+m.ID]; ok {
		t.Fatal("token left in the store after rm")
	}
	// Un teléfono borrado sin kling phone (kling rm, graph rm): ls poda su token.
	if err := a.up(ctx, 1, "phone-golden"); err != nil {
		t.Fatal(err)
	}
	m2, _ := a.phone(ctx, "1")
	_ = f.Remove(ctx, m2.ID)
	if err := a.ls(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store[storeNS+"/"+m2.ID]; ok {
		t.Fatal("orphan token not pruned by ls")
	}
}

// Con la autorización del daemon, un inquilino no tiene /store: el token va a
// un fichero 0600 suyo y todo sigue funcionando.
func TestTokensWithoutStore(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	f.storeForbidden = true
	ctx := context.Background()
	if err := a.up(ctx, 1, "phone-golden"); err != nil {
		t.Fatal(err)
	}
	m, _ := a.phone(ctx, "1")
	p, _ := a.localTokenPath(m.ID)
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("local token file: %v %v", err, st)
	}
	if _, err := a.callPhone(ctx, m, "GET", "/v1/screen", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := a.rm(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("local token left after rm: %v", err)
	}
}

func TestGoldenBuild(t *testing.T) {
	a, f, _ := testApp(t)
	ctx := context.Background()
	info, err := a.goldenBuild(ctx, goldenOpts{Name: "g1", Image: "android13", CPUs: 2, MemMiB: 1536, Egress: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.commits) != 1 || f.commits[0] != "g1" {
		t.Fatalf("commits: %v", f.commits)
	}
	if f.machine("g1-build") != nil {
		t.Fatal("cold machine left behind")
	}
	if !info.Build.OK || info.Build.Files != 1234 || info.Kernel != "6.1.0-android" || info.Arch != "arm64" ||
		info.VerityRootHash == "" {
		t.Fatalf("info: %+v %+v", info, info.Build)
	}
	s, _ := f.Snapshot(ctx, "g1")
	var got goldenInfo
	if ok, err := s.Annotation(annGolden, &got); !ok || err != nil || !got.Build.OK {
		t.Fatalf("annotation: %v %v %+v", ok, err, got)
	}
	if s.Labels[labelPhone] != "1" || s.Labels[api.LabelPorts] != "5555,8091" {
		t.Fatalf("golden labels: %v", s.Labels)
	}
	// verify: un clon de usar y tirar, y la anotación al día.
	v, err := a.goldenVerify(ctx, "g1")
	if err != nil || !v.OK {
		t.Fatalf("verify: %v %+v", err, v)
	}
	if f.machine("g1-verify") != nil {
		t.Fatal("verify clone left behind")
	}
	s, _ = f.Snapshot(ctx, "g1")
	got = goldenInfo{}
	_, _ = s.Annotation(annGolden, &got)
	if got.Verify == nil || !got.Verify.OK {
		t.Fatalf("verify not recorded: %+v", got)
	}
}

// Una caché rota no se guarda: el dorado la repartiría a todos sus clones.
func TestGoldenBuildRefusesBrokenCache(t *testing.T) {
	a, f, _ := testApp(t)
	f.verifyBad = true
	_, err := a.goldenBuild(context.Background(), goldenOpts{Name: "g1", Image: "android13", CPUs: 2, MemMiB: 1536, Egress: "none"})
	if err == nil || !strings.Contains(err.Error(), "broken golden") {
		t.Fatalf("err = %v", err)
	}
	if len(f.commits) != 0 || f.machine("g1-build") != nil {
		t.Fatalf("saved or left behind: %v", f.commits)
	}
}

func TestPoolAndClaim(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	if err := a.poolFill(ctx, "phone-golden", 2); err != nil {
		t.Fatal(err)
	}
	sp, _ := a.spares(ctx, "phone-golden")
	if len(sp) != 2 {
		t.Fatalf("spares: %d", len(sp))
	}
	for _, m := range sp {
		if m.State != api.StatePaused || m.Labels[labelPooled] == "" {
			t.Fatalf("spare %s: %s %v", m.Name, m.State, m.Labels)
		}
	}
	// Otra vez: ya hay dos, no crea más.
	if err := a.poolFill(ctx, "phone-golden", 2); err != nil {
		t.Fatal(err)
	}
	if ps, _ := a.phones(ctx); len(ps) != 2 {
		t.Fatalf("refill created more: %d", len(ps))
	}
	var mu sync.Mutex
	m, how, err := a.claim(ctx, "phone-golden", "mcp-aaa", &mu)
	if err != nil || how != "spare" || m.State != api.StateRunning || f.machine(m.Name).m.Labels[labelOwner] != "mcp-aaa" {
		t.Fatalf("claim: %v %s %+v", err, how, m)
	}
	m2, _, _ := a.claim(ctx, "phone-golden", "mcp-bbb", &mu)
	m3, how3, err := a.claim(ctx, "phone-golden", "mcp-ccc", &mu)
	if err != nil || how3 != "new" || m3.Name == m.Name || m3.Name == m2.Name || m2.Name == m.Name {
		t.Fatalf("third claim: %v %s %s %s %s", err, how3, m.Name, m2.Name, m3.Name)
	}
	// freeze de los repuestos viejos (ya no queda ninguno: todos reclamados).
	if err := a.poolFill(ctx, "phone-golden", 1); err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := a.poolFreeze(ctx, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	sp, _ = a.spares(ctx, "phone-golden")
	if len(sp) != 1 || sp[0].State != api.StateWarm {
		t.Fatalf("old spare not frozen: %+v", sp)
	}
}

// ── MCP ──────────────────────────────────────────────────────────────────────

type mcpClient struct {
	t   *testing.T
	url string
	sid string
}

func (c *mcpClient) rpc(method string, params any) (map[string]any, int) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", c.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.sid = s
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}

func (c *mcpClient) tool(name string, args any) map[string]any {
	c.t.Helper()
	out, code := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if code != 200 || out["error"] != nil {
		c.t.Fatalf("%s: %d %v", name, code, out)
	}
	return out["result"].(map[string]any)
}

func TestMCPOnePhonePerSession(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	s := &mcpServer{a: a, golden: "phone-golden", idle: time.Minute, ttl: time.Hour, sessions: map[string]*mcpSession{}}
	srv := httptest.NewServer(s.handler("/sec/mcp"))
	defer srv.Close()
	ctx := context.Background()

	c1 := &mcpClient{t: t, url: srv.URL + "/sec/mcp"}
	c2 := &mcpClient{t: t, url: srv.URL + "/sec/mcp"}
	for _, c := range []*mcpClient{c1, c2} {
		out, code := c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})
		if code != 200 || c.sid == "" || out["result"] == nil {
			t.Fatalf("initialize: %d %v", code, out)
		}
		lst, _ := c.rpc("tools/list", nil)
		tools := lst["result"].(map[string]any)["tools"].([]any)
		if len(tools) != 8 {
			t.Fatalf("tools: %d", len(tools))
		}
	}
	// Leer el catálogo no crea teléfonos (es lo que hace kling mcp link).
	if ps, _ := a.phones(ctx); len(ps) != 0 {
		t.Fatalf("initialize/tools/list created %d phones", len(ps))
	}
	r := c1.tool("screen", map[string]any{})
	content := r["content"].([]any)
	img := content[0].(map[string]any)
	if b, _ := base64.StdEncoding.DecodeString(img["data"].(string)); img["type"] != "image" || !bytes.Equal(b, fakePNG) {
		t.Fatalf("screen: %v", img)
	}
	p1 := content[1].(map[string]any)["text"].(string)
	tree := c1.tool("tree", map[string]any{})["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(tree, p1) {
		t.Fatalf("tree of the wrong phone: %s (want %s)", tree, p1)
	}
	c1.tool("tap", map[string]any{"x": 100, "y": 200})
	c2.tool("tap", map[string]any{"x": 7, "y": 8})
	p2 := c2.tool("screen", map[string]any{})["content"].([]any)[1].(map[string]any)["text"].(string)
	if p1 == p2 {
		t.Fatalf("two sessions share %s", p1)
	}
	if got := f.phoneOf(p1).taps; len(got) != 1 || got[0] != [2]int{100, 200} {
		t.Fatalf("%s taps: %v", p1, got)
	}
	if got := f.phoneOf(p2).taps; len(got) != 1 || got[0] != [2]int{7, 8} {
		t.Fatalf("%s taps: %v", p2, got)
	}
	// Errores de la herramienta: resultado con isError, no un error de protocolo.
	if r := c1.tool("tap", map[string]any{"x": 1}); r["isError"] != true {
		t.Fatalf("tap without y: %v", r)
	}
	if out, _ := c1.rpc("tools/call", map[string]any{"name": "nope"}); out["error"] == nil {
		t.Fatal("unknown tool accepted")
	}
	// Sesión desconocida: 404 (el gateway rehace el initialize).
	bad := &mcpClient{t: t, url: c1.url, sid: "nope"}
	if _, code := bad.rpc("tools/list", nil); code != 404 {
		t.Fatalf("unknown session: %d", code)
	}
	// Ociosa: su teléfono se pausa y vuelve solo en la siguiente llamada.
	for _, sess := range s.sessions {
		sess.lastUse = time.Now().Add(-2 * time.Minute)
	}
	s.reap(ctx)
	if f.machine(p1).m.State != api.StatePaused {
		t.Fatalf("%s not paused when idle", p1)
	}
	c1.tool("key", map[string]any{"key": "HOME"})
	if f.machine(p1).m.State != api.StateRunning {
		t.Fatalf("%s not resumed", p1)
	}
	// DELETE: la sesión se cierra y su teléfono se borra.
	req, _ := http.NewRequest("DELETE", c1.url, nil)
	req.Header.Set("Mcp-Session-Id", c1.sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("DELETE: %v %v", err, resp)
	}
	if f.machine(p1) != nil {
		t.Fatalf("%s not removed with its session", p1)
	}
	// Caducada: igual.
	for _, sess := range s.sessions {
		sess.lastUse = time.Now().Add(-2 * time.Hour)
	}
	s.reap(ctx)
	if f.machine(p2) != nil || len(s.sessions) != 0 {
		t.Fatalf("expired session kept its phone")
	}
}

func TestMCPRejectsForeignRequests(t *testing.T) {
	a, _, _ := testApp(t)
	s := &mcpServer{a: a, golden: "g", sessions: map[string]*mcpSession{}}
	h := s.handler("/sec/mcp")
	req := httptest.NewRequest("POST", "/sec/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Host = "127.0.0.1:8095"
	req.Header.Set("Content-Type", "text/plain") // lo que manda un formulario de otra web
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 415 {
		t.Fatalf("text/plain: %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/sec/mcp", strings.NewReader(`{}`))
	req.Host = "evil.example:8095" // rebinding de DNS
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("foreign host: %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8095"
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("without the secret path: %d", w.Code)
	}
}

// ── muro ─────────────────────────────────────────────────────────────────────

func TestWall(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	if err := a.up(ctx, 2, "phone-golden"); err != nil {
		t.Fatal(err)
	}
	w := newWall(a, 2)
	h := w.handler(true)
	do := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Host = "127.0.0.1:8765"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if r := do("GET", "/", nil); r.Code != 200 || !strings.Contains(r.Body.String(), "Muro de teléfonos") {
		t.Fatalf("page: %d", r.Code)
	}
	r := do("GET", "/api/phones", nil)
	var ps []map[string]any
	_ = json.Unmarshal(r.Body.Bytes(), &ps)
	if len(ps) != 2 || ps[0]["name"] != "phone-1" || ps[1]["name"] != "phone-2" {
		t.Fatalf("phones: %s", r.Body)
	}
	if r := do("GET", "/shot/phone-2", nil); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), fakePNG) {
		t.Fatalf("shot: %d", r.Code)
	}
	// Sin la cabecera (lo que puede mandar otra web): nada.
	if r := do("POST", "/tap/phone-1?x=10&y=20", nil); r.Code != 403 {
		t.Fatalf("tap without header: %d", r.Code)
	}
	if r := do("POST", "/tap/phone-1?x=10&y=20", map[string]string{wallHeader: "1"}); r.Code != 200 {
		t.Fatalf("tap: %d %s", r.Code, r.Body)
	}
	if r := do("POST", "/key/phone-1?k=RECENTS", map[string]string{wallHeader: "1"}); r.Code != 200 {
		t.Fatalf("key: %d", r.Code)
	}
	if r := do("POST", "/swipe/phone-1?x1=1&y1=2&x2=3&y2=4&ms=100", map[string]string{wallHeader: "1"}); r.Code != 200 {
		t.Fatalf("swipe: %d", r.Code)
	}
	p := f.phoneOf("phone-1")
	if len(p.taps) != 1 || p.taps[0] != [2]int{10, 20} || len(p.keys) != 1 || p.keys[0] != "APP_SWITCH" {
		t.Fatalf("phone-1 got taps %v keys %v", p.taps, p.keys)
	}
	if r := do("POST", "/tap/phone-9?x=1&y=1", map[string]string{wallHeader: "1"}); r.Code != 404 {
		t.Fatalf("unknown phone: %d", r.Code)
	}
	req := httptest.NewRequest("GET", "/api/phones", nil)
	req.Host = "attacker.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("foreign host: %d", rec.Code)
	}
	// Un teléfono pausado no sale en el muro.
	m, _ := a.phone(ctx, "2")
	_, _ = f.Pause(ctx, m.ID)
	w.listAt = time.Time{}
	r = do("GET", "/api/phones", nil)
	ps = nil
	_ = json.Unmarshal(r.Body.Bytes(), &ps)
	if len(ps) != 1 {
		t.Fatalf("paused phone listed: %s", r.Body)
	}
}

func TestNaturalSortAndNames(t *testing.T) {
	ms := []*api.Machine{{Name: "phone-10"}, {Name: "phone-2"}, {Name: "phone-1"}}
	sortNatural(ms)
	if ms[0].Name != "phone-1" || ms[1].Name != "phone-2" || ms[2].Name != "phone-10" {
		t.Fatalf("order: %s %s %s", ms[0].Name, ms[1].Name, ms[2].Name)
	}
	a, _, _ := testApp(t)
	if a.phoneName("3") != "phone-3" || a.phoneName("tel") != "tel" {
		t.Fatal("phoneName")
	}
	for h, want := range map[string]bool{"127.0.0.1:8765": true, "localhost:1": true, "[::1]:80": true,
		"example.com": false, "10.0.0.1:8765": false, "127.0.0.1.nip.io:80": false} {
		if loopbackHost(h) != want {
			t.Errorf("loopbackHost(%q) != %v", h, want)
		}
	}
}

// Un nodo de grafo `from: <dorado>` nace sin identidad ni token (API cerrada);
// adopt se los da.
func TestAdopt(t *testing.T) {
	a, f, _ := testApp(t)
	f.addGolden("phone-golden")
	ctx := context.Background()
	m, err := f.Run(ctx, api.RunRequest{From: "phone-golden", Name: "g-tel"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.call(ctx, m, "x", "GET", "/v1/tree", nil, false); err == nil {
		t.Fatal("a phone without identity answered tree")
	}
	r, err := a.adopt(ctx, "g-tel")
	if err != nil {
		t.Fatal(err)
	}
	if p := f.phoneOf("g-tel"); p.androidID == "" || len(p.tokens) != 1 {
		t.Fatalf("not adopted: %+v", p)
	}
	if _, err := a.callPhone(ctx, r.M, "GET", "/v1/tree", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.adopt(ctx, "g-tel"); err == nil {
		t.Fatal("adopted twice")
	}
	other, _ := f.Run(ctx, api.RunRequest{Image: "min", Name: "web"})
	if _, err := a.adopt(ctx, other.Name); err == nil {
		t.Fatal("adopted a machine that is not a phone")
	}
}
