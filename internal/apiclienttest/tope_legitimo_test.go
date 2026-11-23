package apiclienttest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// R6-I2 (R1/R2): el tope genérico de 16 MiB de Client.do cortaba respuestas
// que el propio contrato admite: POST /machines/{ref}/guest con max_body_bytes
// hasta GuestMaxBodyCap (64 MiB), un snapshot con 32 anotaciones de 1 MiB y las
// listas del agente en su máximo.
func servidorI2(t *testing.T, mux *http.ServeMux) *api.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kl")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return api.NewClient("unix://" + sock)
}

func TestGuestConCuerpoGrandePedido(t *testing.T) {
	const cuerpo = 24 << 20 // dentro de GuestMaxBodyCap
	mux := http.NewServeMux()
	mux.HandleFunc("/machines/m/guest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.GuestResponse{Status: 200, Body: strings.Repeat("a", cuerpo)})
	})
	c := servidorI2(t, mux)
	out, err := c.Guest(context.Background(), "m", api.GuestRequest{Method: "GET", Path: "/", MaxBodyBytes: api.GuestMaxBodyCap})
	if err != nil {
		t.Fatalf("Guest con max_body_bytes=%d y %d bytes de respuesta: %v", api.GuestMaxBodyCap, cuerpo, err)
	}
	if len(out.Body) != cuerpo {
		t.Fatalf("cuerpo = %d bytes", len(out.Body))
	}
}

func TestSnapshotConAnotacionesAlTope(t *testing.T) {
	s := api.Snapshot{Name: "s", Annotations: map[string]json.RawMessage{}}
	v, _ := json.Marshal(strings.Repeat("a", api.MaxValueBytes-2))
	for i := 0; i < api.MaxAnnotations; i++ {
		s.Annotations["k"+string(rune('a'+i%26))+string(rune('a'+i/26))] = v
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots/s", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(s) })
	c := servidorI2(t, mux)
	if _, err := c.Snapshot(context.Background(), "s"); err != nil {
		t.Fatalf("Snapshot con %d anotaciones de %d bytes: %v", api.MaxAnnotations, api.MaxValueBytes, err)
	}
}

// El estado máximo que el agente admite se lista sin ErrResponseTooLarge:
// 64 sesiones con el entorno al tope.
func TestListaDeSesionesAlMaximo(t *testing.T) {
	var lista []api.Session
	entorno := []string{"V=" + strings.Repeat("x", api.SessionMaxEnv-4)}
	for i := 0; i < api.SessionMax; i++ {
		lista = append(lista, api.Session{ID: "s", Env: entorno})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/machines/m/sessions", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(lista) })
	c := servidorI2(t, mux)
	out, err := c.Sessions(context.Background(), "m")
	if err != nil {
		t.Fatalf("Sessions con %d sesiones al tope: %v", api.SessionMax, err)
	}
	if len(out) != api.SessionMax {
		t.Fatalf("sesiones = %d", len(out))
	}
}

// 64 procesos con cmd de hasta 4 KiB por entrada (lo que el agente deja en la lista).
func TestListaDeProcesosAlMaximo(t *testing.T) {
	var lista []api.ProcessInfo
	for i := 0; i < api.ProcessMaxEntries; i++ {
		lista = append(lista, api.ProcessInfo{PID: i + 1, Cmd: []string{strings.Repeat("c", 4<<10)}})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/machines/m/processes", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(lista) })
	c := servidorI2(t, mux)
	out, err := c.Processes(context.Background(), "m")
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(out) != api.ProcessMaxEntries {
		t.Fatalf("procesos = %d", len(out))
	}
}
