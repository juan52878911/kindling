package apiclienttest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una respuesta JSON que viene del invitado no se decodifica sin tope: el
// cliente corta con ErrResponseTooLarge.
func TestRespuestaDeInvitadoSinTope(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "k.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	const viajan = 8 << 20
	trozo := strings.Repeat("{},", 4096)
	mux := http.NewServeMux()
	mux.HandleFunc("/machines/m/processes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("["))
		for n := 0; n < viajan; n += len(trozo) {
			_, _ = w.Write([]byte(trozo))
		}
		_, _ = w.Write([]byte("{}]"))
	})
	mux.HandleFunc("/machines/m/files", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"entries":[`))
		for n := 0; n < 8<<20; n += len(trozo) {
			_, _ = w.Write([]byte(trozo))
		}
		_, _ = w.Write([]byte("{}]}"))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	c := api.NewClient("unix://" + sock)
	ps, err := c.Processes(context.Background(), "m")
	if !errors.Is(err, api.ErrResponseTooLarge) {
		t.Fatalf("Processes: want ErrResponseTooLarge, got %d procesos, err %v", len(ps), err)
	}
	_, err = c.ListFiles(context.Background(), "m", "/")
	if !errors.Is(err, api.ErrResponseTooLarge) {
		t.Fatalf("ListFiles: want ErrResponseTooLarge, got %v", err)
	}
}
