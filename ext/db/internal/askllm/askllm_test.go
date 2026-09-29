package askllm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "sk-ant-test-0123456789abcdef"

func TestFromEnvSinClave(t *testing.T) {
	t.Setenv(EnvKey, "")
	if _, err := FromEnv(""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	t.Setenv(EnvKey, testKey)
	a, err := FromEnv("")
	if err != nil || a.Model != DefaultModel {
		t.Fatalf("FromEnv: %v %v", a, err)
	}
	for _, s := range []string{fmt.Sprint(a), fmt.Sprintf("%+v", a), fmt.Sprintf("%#v", a)} {
		if strings.Contains(s, testKey) {
			t.Fatalf("the key shows when printing: %s", s)
		}
	}
}

func TestComplete(t *testing.T) {
	var got request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("x-api-key") != testKey ||
			r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("content-type") != "application/json" {
			t.Errorf("request %s %v", r.Method, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"SELECT "},{"type":"text","text":"1"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()
	a := New(testKey, "m1")
	a.URL = srv.URL
	out, err := a.Complete(context.Background(), "sys", "q")
	if err != nil || out != "SELECT 1" {
		t.Fatalf("Complete = %q, %v", out, err)
	}
	if got.Model != "m1" || got.System != "sys" || len(got.Messages) != 1 || got.Messages[0].Content != "q" {
		t.Fatalf("body %+v", got)
	}
}

func TestErrorSinClave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()
	a := New(testKey, "")
	a.URL = srv.URL
	_, err := a.Complete(context.Background(), "", "q")
	if err == nil || !strings.Contains(err.Error(), "authentication_error") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("the key is in the error")
	}
}

func TestNoSigueRedirecciones(t *testing.T) {
	otro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			t.Error("the key followed a redirect")
		}
	}))
	defer otro.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otro.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	a := New(testKey, "")
	a.URL = srv.URL
	if _, err := a.Complete(context.Background(), "", "q"); err == nil {
		t.Fatal("want error on a redirect")
	}
}
