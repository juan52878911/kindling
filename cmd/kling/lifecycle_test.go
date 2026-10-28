package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// `kling rm a b c` con b fallando sigue con c, cuenta el error de b en stderr
// y sale con 1 al final, como docker.
func TestLifecycleSigueTrasUnFallo(t *testing.T) {
	var mu sync.Mutex
	var vistos []string
	m := http.NewServeMux()
	m.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		ref := r.PathValue("ref")
		mu.Lock()
		vistos = append(vistos, ref)
		mu.Unlock()
		if ref == "b" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"machine \"b\" doesn't exist"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := fakeDaemon(t, m)

	var out, errw bytes.Buffer
	err := runLifecycle(context.Background(), c, "rm", []string{"a", "b", "c"}, &out, &errw)
	if err == nil || codigoDeSalida(err) != 1 || err.Error() != "" {
		t.Fatalf("err = %v; quería salir con 1 sin repetir el error", err)
	}
	mu.Lock()
	pedidos := strings.Join(vistos, ",")
	mu.Unlock()
	if pedidos != "a,b,c" {
		t.Fatalf("pedidos = %s; quería a,b,c", pedidos)
	}
	if out.String() != "a\nc\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.Contains(errw.String(), `b: machine "b" doesn't exist`) {
		t.Errorf("stderr = %q", errw.String())
	}

	// Con una sola ref, el error sale tal cual (con su pista y su código).
	errw.Reset()
	if err := runLifecycle(context.Background(), c, "rm", []string{"b"}, &out, &errw); err == nil || err.Error() == "" || errw.Len() != 0 {
		t.Fatalf("una ref: err = %v, stderr = %q", err, errw.String())
	}
	// Todo bien: sin error.
	if err := runLifecycle(context.Background(), c, "rm", []string{"a", "c"}, &out, &errw); err != nil {
		t.Fatalf("todo bien: %v", err)
	}
}
