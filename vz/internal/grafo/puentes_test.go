package grafo

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

// Un puente al host: la conexión del invitado al puerto de la pasarela se
// pide al daemon como KindLoopback con el puerto del HOST (nunca una
// dirección), y llega al servicio del loopback. Sin puente, el puerto no es
// de nadie.
func TestServePuente(t *testing.T) {
	dest := eco(t)
	_, sp, _ := net.SplitHostPort(dest)
	hp, _ := strconv.Atoi(sp)
	d := &daemonFalso{t: t, decidir: func(r linkbroker.Request) (string, error) {
		if r.Kind == linkbroker.KindLoopback && r.Port == hp {
			return dest, nil
		}
		return "", io.ErrUnexpectedEOF
	}}
	audPath := filepath.Join(t.TempDir(), credproxy.AuditFile)
	aud := credproxy.NewAuditor(audPath, nil)
	g := NewConDial(d.dial, aud, nil)
	if g.Link(11434) || g.HayPuentes() {
		t.Fatal("sin puentes, el puerto ya era de alguien")
	}
	if err := g.SetPuentes([]Puente{{Host: hp, Guest: 11434}}); err != nil {
		t.Fatal(err)
	}
	if !g.Link(11434) || !g.HayPuentes() || g.Link(uint16(hp)) {
		t.Fatal("Link no refleja el puente (por el puerto del invitado, no el del host)")
	}

	invitado, red := parTCP(t)
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		g.Serve(context.Background(), 11434, func() (net.Conn, error) { return red, nil }, func() { t.Error("rechazada") })
	}()
	if _, err := invitado.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(invitado, b); err != nil || string(b) != "ping" {
		t.Fatalf("eco: %q %v", b, err)
	}
	invitado.Close()
	<-hecho

	d.mu.Lock()
	if len(d.pedidas) != 1 || d.pedidas[0].Kind != linkbroker.KindLoopback || d.pedidas[0].Port != hp || d.pedidas[0].Host != "" {
		t.Errorf("petición al daemon: %+v", d.pedidas)
	}
	d.mu.Unlock()

	// Quitarlos: el puerto deja de atenderse.
	if err := g.SetPuentes(nil); err != nil || g.Link(11434) || g.HayPuentes() {
		t.Fatalf("quitar: %v", err)
	}

	aud.Close()
	raw, err := os.ReadFile(audPath)
	if err != nil {
		t.Fatal(err)
	}
	var r credproxy.Record
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &r)
	if r.Kind != credproxy.KindLink || r.Host != "host.kindling.internal:11434" || r.Upstream != "host:127.0.0.1:"+sp || r.ReqBytes != 4 {
		t.Errorf("auditoría: %+v", r)
	}
}

// Las reglas de los puentes: en rango, fuera de los puertos de la pasarela y
// del rango de reenvíos, sin repetir y sin pisar una arista link.
func TestSetPuentesValida(t *testing.T) {
	g := NewConDial(nil, nil, nil)
	if err := g.Set([]Enlace{{Host: "api.graph", Port: 9000}}); err != nil {
		t.Fatal(err)
	}
	for _, ps := range [][]Puente{
		{{Host: 0, Guest: 1}},
		{{Host: 11434, Guest: 53}},
		{{Host: 11434, Guest: 80}},
		{{Host: 11434, Guest: 443}},
		{{Host: credproxy.ForwardPortMin, Guest: 7000}},
		{{Host: 1, Guest: 7000}, {Host: 2, Guest: 7000}},
		{{Host: 11434, Guest: 9000}},
	} {
		if err := g.SetPuentes(ps); err == nil {
			t.Errorf("aceptó %+v", ps)
		}
	}
	if err := g.SetPuentes([]Puente{{Host: 8080, Guest: 8080}, {Host: 11434, Guest: 11434}}); err != nil {
		t.Fatal(err)
	}
}
