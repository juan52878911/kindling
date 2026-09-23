package daemon

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

func tarCon(t *testing.T, name string, body []byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	return &buf
}

func TestSubidaDeCarpetaCodigos(t *testing.T) {
	s, _ := servidorBlobs(t)
	h := s.routes()
	subir := func(body *bytes.Buffer) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/shares/uploads", body))
		return w
	}
	if w := subir(tarCon(t, "../escape", []byte("x"))); w.Code != http.StatusBadRequest {
		t.Errorf("tar with ..: %d %s", w.Code, w.Body)
	}
	if w := subir(bytes.NewBufferString("not a tar at all, just some bytes that go on and on")); w.Code != http.StatusBadRequest {
		t.Errorf("garbage: %d %s", w.Code, w.Body)
	}
	s.SetShareConfig(func() machine.ShareConfig { return machine.ShareConfig{CopyMaxBytes: 4} })
	if w := subir(tarCon(t, "big", make([]byte, 64))); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d %s", w.Code, w.Body)
	}
}

func TestRunConCarpetasCodigos(t *testing.T) {
	s, _ := servidorBlobs(t)
	h := s.routes()
	run := func(req api.RunRequest) *httptest.ResponseRecorder {
		b, _ := json.Marshal(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/machines", bytes.NewReader(b)))
		return w
	}
	// Con from: las carpetas se piden al arrancar en frío.
	w := run(api.RunRequest{From: "x", Shares: []api.ShareSpec{{Mount: "/w", Upload: strings.Repeat("a", 32)}}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cold boot") {
		t.Errorf("run -from with shares: %d %s", w.Code, w.Body)
	}
	// Vivas sin raíces permitidas: 400 que dice cómo permitirlas.
	w = run(api.RunRequest{Image: "min", Shares: []api.ShareSpec{{Mode: "rw", Mount: "/w", Source: t.TempDir()}}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "share_roots") {
		t.Errorf("live share without roots: %d %s", w.Code, w.Body)
	}
	// El commit con carpetas es 409 (ver machine.TestCommitConCarpetaSeRechaza
	// para el rechazo en sí): aquí, que el código sea ese.
	if c := runStatus(machine.ErrShareRequest); c != http.StatusBadRequest {
		t.Errorf("ErrShareRequest -> %d", c)
	}
}
