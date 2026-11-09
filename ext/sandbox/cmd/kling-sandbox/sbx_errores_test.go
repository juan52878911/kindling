package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frontalQueContesta apunta el cliente a un frontal que contesta siempre lo
// mismo.
func frontalQueContesta(t *testing.T, code int, cabeceras map[string]string, cuerpo string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range cabeceras {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, cuerpo)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KLING_SANDBOX_URL", srv.URL)
	t.Setenv("KLING_SANDBOX_TOKEN", "t")
}

// salida ejecuta f y devuelve lo que escribió en stdout.
func salida(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	viejo := os.Stdout
	os.Stdout = w
	ferr := f()
	os.Stdout = viejo
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b), ferr
}

// Un 429 con Retry-After no se aplana a un texto: el error conserva el código
// y la espera, y con -json sale en stdout para que un agente lo lea.
func TestNewConservaElCodigoYElRetryAfter(t *testing.T) {
	frontalQueContesta(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "30"},
		`{"message":"tenant a already has 3 sandboxes; the limit is 3"}`)

	out, err := salida(t, func() error { return sbxNew([]string{"-image", "base", "-json"}) })
	var ef *errFrontal
	if !errors.As(err, &ef) || ef.Status != http.StatusTooManyRequests || ef.RetryAfter != 30 {
		t.Fatalf("error: %#v", err)
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "30s") {
		t.Fatalf("the message lost the status or the wait: %v", err)
	}
	var j struct {
		Error struct {
			Status     int    `json:"status"`
			Message    string `json:"message"`
			RetryAfter int    `json:"retry_after"`
		} `json:"error"`
	}
	if e := json.Unmarshal([]byte(out), &j); e != nil {
		t.Fatalf("stdout is not JSON: %q", out)
	}
	if j.Error.Status != 429 || j.Error.RetryAfter != 30 || !strings.Contains(j.Error.Message, "limit is 3") {
		t.Fatalf("json error: %+v", j)
	}
}

func TestNewJSONDevuelveElSandboxEntero(t *testing.T) {
	frontalQueContesta(t, http.StatusCreated, nil, `{"id":"h/abc","host":"h","state":"running","labels":{"k":"v"}}`)
	out, err := salida(t, func() error { return sbxNew([]string{"-image", "base", "-json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var sb map[string]any
	if e := json.Unmarshal([]byte(out), &sb); e != nil || sb["id"] != "h/abc" || sb["labels"] == nil {
		t.Fatalf("stdout: %q", out)
	}
}

func TestRmJSONDistingueUnNoExiste(t *testing.T) {
	frontalQueContesta(t, http.StatusNotFound, nil, `{"message":"no sandbox \"h/x\""}`)
	out, err := salida(t, func() error { return sbxRm([]string{"-json", "h/x"}) })
	if err == nil {
		t.Fatal("expected an error")
	}
	var j []struct {
		ID      string `json:"id"`
		Removed bool   `json:"removed"`
		Error   *struct {
			Status int `json:"status"`
		} `json:"error"`
	}
	if e := json.Unmarshal([]byte(out), &j); e != nil {
		t.Fatalf("stdout is not JSON: %q", out)
	}
	if len(j) != 1 || j[0].ID != "h/x" || j[0].Removed || j[0].Error == nil || j[0].Error.Status != 404 {
		t.Fatalf("rm -json: %s", out)
	}
}
