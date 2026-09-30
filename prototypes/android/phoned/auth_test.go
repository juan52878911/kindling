package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sumHex(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

func doAuth(t *testing.T, h http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// Sin tokens, la API está cerrada: solo /v1/health (y el índice) contestan.
// Es lo que tiene un dorado y un nodo de grafo hecho de él sin identidad.
func TestAuthLockedWithoutTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-tokens.json")
	f := &fakeOps{running: true}
	h := newAPI(f, newAuthz(path))
	if w := doAuth(t, h, "GET", "/v1/health", "", ""); w.Code != 200 {
		t.Fatalf("health must stay open: %d", w.Code)
	}
	var hi healthInfo
	_ = json.Unmarshal(doAuth(t, h, "GET", "/v1/health", "", "").Body.Bytes(), &hi)
	if hi.APITokens != 0 {
		t.Fatalf("api_tokens = %d, want 0", hi.APITokens)
	}
	for _, r := range [][2]string{{"GET", "/v1/screen"}, {"GET", "/v1/tree"}, {"POST", "/v1/tap"},
		{"POST", "/v1/install"}, {"GET", "/v1/identity"}, {"GET", "/v1/logs"}, {"POST", "/v1/verify-cache"}} {
		w := doAuth(t, h, r[0], r[1], "anything", `{"x":1,"y":2}`)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "locked") {
			t.Fatalf("%s %s without tokens: %d %s", r[0], r[1], w.Code, w.Body)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%s %s: no WWW-Authenticate", r[0], r[1])
		}
	}
	if len(f.inputs) != 0 {
		t.Fatalf("a locked API must not touch the phone: %v", f.inputs)
	}
}

// Una arista sin token (o con uno cualquiera) recibe 401; el de control abre
// todo; uno de lectura solo screen y tree.
func TestAuthScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-tokens.json")
	if err := writeTokens(path, []apiToken{{SHA256: sumHex("ctl-token"), Scope: scopeControl},
		{SHA256: sumHex("ro-token"), Scope: scopeRead}}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("tokens file mode %v, want 0600", st.Mode().Perm())
	}
	f := &fakeOps{running: true}
	h := newAPI(f, newAuthz(path))

	if w := doAuth(t, h, "POST", "/v1/tap", "", `{"x":1,"y":2}`); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := doAuth(t, h, "POST", "/v1/tap", "wrong", `{"x":1,"y":2}`); w.Code != 401 ||
		strings.Contains(w.Body.String(), "locked") {
		t.Fatalf("wrong token: %d %s", w.Code, w.Body)
	}
	if w := doAuth(t, h, "POST", "/v1/tap", "ctl-token", `{"x":1,"y":2}`); w.Code != 200 {
		t.Fatalf("control token: %d %s", w.Code, w.Body)
	}
	if w := doAuth(t, h, "GET", "/v1/screen", "ro-token", ""); w.Code != 200 {
		t.Fatalf("read token on screen: %d", w.Code)
	}
	if w := doAuth(t, h, "GET", "/v1/tree", "ro-token", ""); w.Code != 200 {
		t.Fatalf("read token on tree: %d", w.Code)
	}
	for _, r := range [][2]string{{"POST", "/v1/tap"}, {"GET", "/v1/identity"}, {"GET", "/v1/logs"}, {"POST", "/v1/install"}} {
		if w := doAuth(t, h, r[0], r[1], "ro-token", `{"x":1,"y":2}`); w.Code != 403 {
			t.Fatalf("read token on %s %s: %d", r[0], r[1], w.Code)
		}
	}
	// El esquema es insensible a mayúsculas; el token no.
	req := httptest.NewRequest("GET", "/v1/screen", nil)
	req.Header.Set("Authorization", "bearer ctl-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("lowercase bearer: %d", w.Code)
	}
	if w := doAuth(t, h, "GET", "/v1/screen", "CTL-TOKEN", ""); w.Code != 401 {
		t.Fatalf("token must be case-sensitive: %d", w.Code)
	}
	if len(f.inputs) != 1 {
		t.Fatalf("only the control tap must reach the phone: %v", f.inputs)
	}
	var hi healthInfo
	_ = json.Unmarshal(doAuth(t, h, "GET", "/v1/health", "", "").Body.Bytes(), &hi)
	if hi.APITokens != 2 {
		t.Fatalf("api_tokens = %d, want 2", hi.APITokens)
	}
}

// El gancho de identidad (otro proceso) reescribe el fichero: el servidor lo
// relee sin reiniciar, y revocar (lista vacía) vuelve a cerrar.
func TestAuthReloadAndRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-tokens.json")
	az := newAuthz(path)
	h := newAPI(&fakeOps{running: true}, az)
	if w := doAuth(t, h, "GET", "/v1/screen", "a", ""); w.Code != 401 {
		t.Fatalf("before: %d", w.Code)
	}
	if err := writeTokens(path, []apiToken{{SHA256: sumHex("a"), Scope: scopeControl}}); err != nil {
		t.Fatal(err)
	}
	if w := doAuth(t, h, "GET", "/v1/screen", "a", ""); w.Code != 200 {
		t.Fatalf("after write: %d", w.Code)
	}
	// Mismo tamaño, otro contenido: el mtime tiene que moverse.
	time.Sleep(20 * time.Millisecond)
	if err := writeTokens(path, []apiToken{{SHA256: sumHex("b"), Scope: scopeControl}}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(path, later, later)
	if w := doAuth(t, h, "GET", "/v1/screen", "a", ""); w.Code != 401 {
		t.Fatalf("old token after rotation: %d", w.Code)
	}
	if w := doAuth(t, h, "GET", "/v1/screen", "b", ""); w.Code != 200 {
		t.Fatalf("new token after rotation: %d", w.Code)
	}
	if err := writeTokens(path, []apiToken{}); err != nil {
		t.Fatal(err)
	}
	if w := doAuth(t, h, "GET", "/v1/screen", "b", ""); w.Code != 401 {
		t.Fatalf("after revoke: %d", w.Code)
	}
	// Un fichero que no se entiende cierra, no abre.
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if w := doAuth(t, h, "GET", "/v1/screen", "b", ""); w.Code != 401 {
		t.Fatalf("garbage file: %d", w.Code)
	}
}

func TestIdentityAPITokens(t *testing.T) {
	doc := `{"phone":{"android_id":"0123456789abcdef","api_tokens":[{"sha256":"` + sumHex("x") + `","scope":"control"}]}}`
	id, err := parseMMDS([]byte(doc))
	if err != nil || id == nil || id.APITokens == nil || len(*id.APITokens) != 1 || id.authOnly() {
		t.Fatalf("identity with tokens: %+v %v", id, err)
	}
	// Los tokens no cambian la huella de la identidad: rotarlos no la rehace.
	doc2 := `{"phone":{"android_id":"0123456789abcdef"}}`
	id2, _ := parseMMDS([]byte(doc2))
	if id.digest() != id2.digest() {
		t.Fatal("api_tokens must not change the identity digest")
	}
	// Solo tokens: auth-only, sin android_id.
	id3, err := parseMMDS([]byte(`{"phone":{"api_tokens":[]}}`))
	if err != nil || id3 == nil || !id3.authOnly() {
		t.Fatalf("auth-only doc: %+v %v", id3, err)
	}
	// Nunca el token en claro, ni un ámbito inventado.
	for _, bad := range []string{
		`{"phone":{"api_tokens":[{"sha256":"secret-token","scope":"control"}]}}`,
		`{"phone":{"api_tokens":[{"sha256":"` + sumHex("x") + `","scope":"root"}]}}`,
		`{"phone":{"name":"p1","api_tokens":[]}}`, // no es auth-only: pide android_id
	} {
		if _, err := parseMMDS([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestRequiredScope(t *testing.T) {
	cases := map[[2]string]string{
		{"GET", "/v1/health"}:          scopeNone,
		{"GET", "/"}:                   scopeNone,
		{"GET", "/v1/screen"}:          scopeRead,
		{"GET", "/v1/tree"}:            scopeRead,
		{"POST", "/v1/screen"}:         scopeControl,
		{"POST", "/v1/health"}:         scopeControl,
		{"GET", "/v1/identity"}:        scopeControl,
		{"GET", "/v1/logs"}:            scopeControl,
		{"POST", "/v1/verify-cache"}:   scopeControl,
		{"GET", "/v1/health/../tree"}:  scopeControl,
		{"GET", "/v1/nonexistent"}:     scopeControl,
		{"POST", "/v1/install"}:        scopeControl,
		{"DELETE", "/v1/health"}:       scopeControl,
		{"GET", "/v1/tree/"}:           scopeControl,
		{"GET", "/V1/SCREEN"}:          scopeControl,
		{"OPTIONS", "/v1/screen"}:      scopeControl,
		{"GET", "//v1/health"}:         scopeControl,
		{"HEAD", "/v1/health"}:         scopeControl,
		{"GET", "/v1/screen\x00"}:      scopeControl,
		{"POST", "/v1/verify-cache/x"}: scopeControl,
	}
	for k, want := range cases {
		if got := requiredScope(k[0], k[1]); got != want {
			t.Errorf("%s %q: %q, want %q", k[0], k[1], got, want)
		}
	}
}
