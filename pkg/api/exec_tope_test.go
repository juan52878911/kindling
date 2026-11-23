package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const topeDePrueba = 64 << 10

// daemonQueSePasa contesta al exec con el doble del tope pedido en cada flujo:
// un daemon que no cumple su parte.
func daemonQueSePasa(tb testing.TB) *Client {
	tb.Helper()
	trozo := strings.Repeat("A", 32<<10)
	out, _ := json.Marshal(ExecEvent{Stream: "stdout", Data: []byte(trozo)})
	errEv, _ := json.Marshal(ExecEvent{Stream: "stderr", Data: []byte(trozo)})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/{ref}/exec", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i := 0; i < 2*topeDePrueba/len(trozo); i++ {
			_, _ = w.Write(append(out, '\n'))
			_, _ = w.Write(append(errEv, '\n'))
		}
		_, _ = io.WriteString(w, `{"exit":0}`+"\n")
	})
	return clienteDePrueba(tb, mux)
}

// Client.Exec no guarda más del tope pedido aunque el otro extremo mande más,
// y lo marca como truncado.
func TestExecNoPasaDelTopeAunqueElDaemonSiga(t *testing.T) {
	c := daemonQueSePasa(t)
	res, err := c.Exec(context.Background(), "m", ExecRequest{Cmd: []string{"x"}, MaxOutputBytes: topeDePrueba}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != topeDePrueba || len(res.Stderr) != topeDePrueba || !res.Truncated {
		t.Fatalf("stdout %d, stderr %d, truncated %v; quería %d, %d y true",
			len(res.Stdout), len(res.Stderr), res.Truncated, topeDePrueba, topeDePrueba)
	}
}

// BenchmarkExecSalidaDeMas mide cuánta salida acumula Client.Exec (sin
// onOutput) cuando el otro extremo manda el doble del tope pedido (B-16), en
// veces el tope.
func BenchmarkExecSalidaDeMas(b *testing.B) {
	c := daemonQueSePasa(b)
	var guardado int
	for i := 0; i < b.N; i++ {
		res, err := c.Exec(context.Background(), "m", ExecRequest{Cmd: []string{"x"}, MaxOutputBytes: topeDePrueba}, nil)
		if err != nil {
			b.Fatal(err)
		}
		guardado += len(res.Stdout)
	}
	b.ReportMetric(float64(guardado)/float64(b.N)/topeDePrueba, "x-cap/op")
}
