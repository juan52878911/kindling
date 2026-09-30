package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeOps struct {
	inputs  []inputReq
	apk     []byte
	running bool
}

var fakePNG = []byte("\x89PNG\r\n\x1a\n\x00\x01\xfe\xffbinary")

func (f *fakeOps) Health(context.Context) healthInfo {
	return healthInfo{OK: f.running, State: "running", BootCompleted: f.running, Verity: "none"}
}
func (f *fakeOps) Screen(context.Context) ([]byte, error) {
	if !f.running {
		return nil, errNotRunning
	}
	return fakePNG, nil
}
func (f *fakeOps) Tree(_ context.Context, c bool) ([]byte, string, error) {
	return []byte("<hierarchy compressed=\"" + map[bool]string{true: "1", false: "0"}[c] + "\"/>"), "uidump", nil
}
func (f *fakeOps) Input(_ context.Context, in inputReq) (string, error) {
	f.inputs = append(f.inputs, in)
	return "uidump", nil
}
func (f *fakeOps) Install(_ context.Context, apk []byte) (string, error) {
	f.apk = apk
	return "Success", nil
}
func (f *fakeOps) Launch(_ context.Context, pkg string) (string, error) {
	return "Status: ok " + pkg, nil
}
func (f *fakeOps) Logs(_ context.Context, b string, n int) ([]byte, error) {
	return []byte(b + "\n"), nil
}
func (f *fakeOps) Identity(context.Context) (identityInfo, error) {
	return identityInfo{Serial: "S1"}, errors.New("x")
}
func (f *fakeOps) VerifyCache(context.Context) (verifyResult, error) {
	return verifyResult{OK: true, Files: 3}, nil
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAPIScreenAndEncoding(t *testing.T) {
	f := &fakeOps{running: true}
	h := newAPI(f, nil)
	w := do(t, h, "GET", "/v1/screen", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), fakePNG) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("raw screen: %d %q", w.Code, w.Body.Bytes())
	}
	w = do(t, h, "GET", "/v1/screen?encoding=base64", "")
	got, err := base64.StdEncoding.DecodeString(w.Body.String())
	if w.Code != 200 || err != nil || !bytes.Equal(got, fakePNG) {
		t.Fatalf("base64 screen: %d %v", w.Code, err)
	}
	f.running = false
	if w = do(t, h, "GET", "/v1/screen", ""); w.Code != 503 {
		t.Fatalf("screen while not running: %d", w.Code)
	}
	if w = do(t, h, "GET", "/v1/health", ""); w.Code != 503 {
		t.Fatalf("health while not running: %d", w.Code)
	}
	if w = do(t, h, "GET", "/v1/tree?compressed=1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"1"`) ||
		w.Header().Get("X-Phoned-Source") != "uidump" {
		t.Fatalf("tree: %d %s", w.Code, w.Body)
	}
}

func TestAPIInputValidation(t *testing.T) {
	f := &fakeOps{running: true}
	h := newAPI(f, nil)
	ok := []struct{ path, body string }{
		{"/v1/tap", `{"x":10,"y":20}`},
		{"/v1/swipe", `{"x1":1,"y1":2,"x2":3,"y2":4}`},
		{"/v1/text", `{"text":"hola mundo; rm -rf /"}`},
		{"/v1/key", `{"key":"back"}`},
		{"/v1/key", `{"key":4}`},
		{"/v1/key", `{"key":"KEYCODE_HOME"}`},
	}
	for _, c := range ok {
		if w := do(t, h, "POST", c.path, c.body); w.Code != 200 {
			t.Fatalf("%s %s: %d %s", c.path, c.body, w.Code, w.Body)
		}
	}
	if f.inputs[1].MS != 300 || f.inputs[3].Key != "BACK" || f.inputs[4].Key != "4" {
		t.Fatalf("parsed: %+v", f.inputs)
	}
	bad := []struct{ path, body string }{
		{"/v1/tap", `{"x":10}`},
		{"/v1/tap", `{"x":-1,"y":2}`},
		{"/v1/tap", `nope`},
		{"/v1/swipe", `{"x1":1,"y1":2,"x2":3,"y2":4,"ms":20000}`},
		{"/v1/text", `{"text":"a\nb"}`},
		{"/v1/text", `{"text":""}`},
		{"/v1/key", `{"key":"BACK; reboot"}`},
		{"/v1/key", `{"key":"$(id)"}`},
	}
	for _, c := range bad {
		if w := do(t, h, "POST", c.path, c.body); w.Code != 400 {
			t.Fatalf("%s %s: %d (want 400)", c.path, c.body, w.Code)
		}
	}
	if w := do(t, h, "GET", "/v1/tap", ""); w.Code != 405 {
		t.Fatalf("GET /v1/tap: %d", w.Code)
	}
}

func TestAPIInstall(t *testing.T) {
	f := &fakeOps{running: true}
	h := newAPI(f, nil)
	apk := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0, 0xff, 7}, 5000)...)
	b64 := base64.StdEncoding.EncodeToString(apk)
	// Con saltos de línea, como `base64` de coreutils.
	var wrapped strings.Builder
	for i := 0; i < len(b64); i += 76 {
		e := min(i+76, len(b64))
		wrapped.WriteString(b64[i:e] + "\n")
	}
	w := do(t, h, "POST", "/v1/install?encoding=base64", wrapped.String())
	if w.Code != 200 || !bytes.Equal(f.apk, apk) {
		t.Fatalf("install b64: %d %s (got %d bytes)", w.Code, w.Body, len(f.apk))
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Fatalf("install result %v", res)
	}
	if w = do(t, h, "POST", "/v1/install", string(apk)); w.Code != 200 {
		t.Fatalf("install raw: %d", w.Code)
	}
	if w = do(t, h, "POST", "/v1/install", "not a zip"); w.Code != 400 {
		t.Fatalf("install garbage: %d", w.Code)
	}
	if w = do(t, h, "POST", "/v1/install?encoding=base64", "!!!!"); w.Code != 400 {
		t.Fatalf("install bad base64: %d", w.Code)
	}
}

func TestAPILogsAndIndex(t *testing.T) {
	h := newAPI(&fakeOps{running: true}, nil)
	if w := do(t, h, "GET", "/v1/logs?buffer=crash&lines=5", ""); w.Code != 200 || w.Body.String() != "crash\n" {
		t.Fatalf("logs: %d %q", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/v1/launch", `{"package":"com.termux"}`); w.Code != 200 {
		t.Fatalf("launch: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{`{"package":"com.termux; reboot"}`, `{"package":"termux"}`, `{}`} {
		if w := do(t, h, "POST", "/v1/launch", bad); w.Code != 400 {
			t.Fatalf("launch %s: %d", bad, w.Code)
		}
	}
	for _, q := range []string{"buffer=kernel", "lines=0", "lines=99999", "lines=x"} {
		if w := do(t, h, "GET", "/v1/logs?"+q, ""); w.Code != 400 {
			t.Fatalf("logs?%s: %d", q, w.Code)
		}
	}
	w := do(t, h, "GET", "/", "")
	body, _ := io.ReadAll(w.Body)
	if w.Code != 200 || !strings.Contains(string(body), "/v1/screen") {
		t.Fatalf("index: %d %s", w.Code, body)
	}
	if w := do(t, h, "GET", "/v1/identity", ""); w.Code != 500 {
		t.Fatalf("identity error: %d", w.Code)
	}
	if w := do(t, h, "GET", "/exec", ""); w.Code != 404 {
		t.Fatalf("unknown route: %d", w.Code)
	}
}
