package daemon

// El túnel TCP a un puerto del invitado: GET /machines/{ref}/tcp?port=N con
// Upgrade: kling-tcp/1. Tras el 101 el daemon copia bytes en los dos sentidos
// entre la conexión del cliente y el puerto N de la máquina, por el mismo
// camino que usa la publicación (mc.Addr: la IP del tap en Linux, el reenvío
// de loopback en macOS). Es lo que necesita quien llega por SSH a un host
// remoto y quiere WebSocket o binario, que el proxy /guest (HTTP de texto,
// 1 MiB) no sirve.
//
// Solo puertos del invitado: nunca se marca nada que no sea mc.Addr(port), y
// ese port tiene que estar en la etiqueta kling.ports (los mismos que el
// daemon ya trata como expuestos). El del agente (api.GuestPort) no: sería
// saltarse el relé de la shell y el exec.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func init() { Capabilities = append(Capabilities, api.CapabilityTCPTunnel) }

const (
	// tunelesMaxMaquina: túneles abiertos a la vez por máquina.
	tunelesMaxMaquina = 256
	// tunelInactividad: sin bytes en ningún sentido durante este tiempo, se
	// cierra. Un WebSocket vivo manda pings; uno colgado no retiene la plaza.
	tunelInactividad = 10 * time.Minute
	// tunelPlazoMarcar: lo que se reintenta conectar con un servicio que aún
	// no escucha (recién despertado de un freeze).
	tunelPlazoMarcar = 15 * time.Second
)

// topesTunel son los topes de un servidor; cero = el valor por defecto. Los
// tests los bajan en su Server en vez de tocar variables globales.
type topesTunel struct {
	max                int
	inactividad, plazo time.Duration
}

func (t topesTunel) conDefectos() topesTunel {
	if t.max == 0 {
		t.max = tunelesMaxMaquina
	}
	if t.inactividad == 0 {
		t.inactividad = tunelInactividad
	}
	if t.plazo == 0 {
		t.plazo = tunelPlazoMarcar
	}
	return t
}

// tunelesAbiertos cuenta los túneles por id de máquina.
type tunelesAbiertos struct {
	mu sync.Mutex
	n  map[string]int
}

func (t *tunelesAbiertos) tomar(id string, tope int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n == nil {
		t.n = map[string]int{}
	}
	if t.n[id] >= tope {
		return false
	}
	t.n[id]++
	return true
}

func (t *tunelesAbiertos) soltar(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n[id]--; t.n[id] <= 0 {
		delete(t.n, id)
	}
}

func (s *Server) handleTCP(w http.ResponseWriter, r *http.Request) {
	topes := s.topesTunel.conDefectos()
	if !strings.EqualFold(r.Header.Get("Upgrade"), api.TCPProto) {
		fail(w, http.StatusUpgradeRequired, fmt.Errorf("this endpoint speaks %s; ask for it with Upgrade", api.TCPProto))
		return
	}
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		fail(w, http.StatusBadRequest, errors.New("port: a guest port (1-65535) is required"))
		return
	}
	mc, ok := s.mgr.Get(r.PathValue("ref"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("that machine doesn't exist"))
		return
	}
	if port == api.GuestPort || !slices.Contains(mc.ExposedPorts(), port) {
		fail(w, http.StatusForbidden, fmt.Errorf("port %d is not published by %s: list it in its %s label", port, mc.Name, api.LabelPorts))
		return
	}
	if !s.tuneles.tomar(mc.ID, topes.max) {
		w.Header().Set("Retry-After", guestBusyRetryAfter)
		fail(w, http.StatusTooManyRequests, fmt.Errorf("%s has %d tunnels open (the limit); retry when one ends", mc.Name, topes.max))
		return
	}
	defer s.tuneles.soltar(mc.ID)

	// Primero el invitado y después el secuestro: mientras la respuesta siga
	// siendo HTTP normal, un fallo se cuenta con un código y un mensaje.
	guest, code, err := s.marcarInvitado(r.Context(), mc.ID, port, topes.plazo)
	if err != nil {
		fail(w, code, err)
		return
	}
	defer guest.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusInternalServerError, errors.New("this server cannot take over the connection"))
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
		api.TCPProto + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}
	// Lo que el cliente ya mandó y quedó en el búfer del servidor HTTP.
	var cli io.Reader = conn
	if n := brw.Reader.Buffered(); n > 0 {
		pre, _ := brw.Reader.Peek(n)
		cli = io.MultiReader(strings.NewReader(string(pre)), conn)
	}
	relayTCP(r.Context(), conn, cli, guest, topes.inactividad)
}

// despertarMaquina descongela id; los tests lo sustituyen (s.thaw).
func (s *Server) despertarMaquina(ctx context.Context, id string) (*api.Machine, error) {
	if s.thaw != nil {
		return s.thaw(ctx, id)
	}
	return s.mgr.Thaw(ctx, id)
}

// marcarInvitado despierta la máquina si hace falta y conecta con su puerto,
// reintentando un rato si el servicio aún no escucha.
func (s *Server) marcarInvitado(ctx context.Context, id string, port int, plazo time.Duration) (net.Conn, int, error) {
	limite := time.Now().Add(plazo)
	paso := 5 * time.Millisecond
	var ultimo error
	for {
		mc, ok := s.mgr.Get(id)
		if !ok {
			return nil, http.StatusNotFound, errors.New("that machine doesn't exist")
		}
		if mc.State == api.StateWarm || mc.State == api.StatePaused {
			out, err := s.despertarMaquina(ctx, id)
			if err != nil {
				return nil, execStatus(err), fmt.Errorf("waking %s up: %w", mc.Name, err)
			}
			mc = out
		}
		if mc.State != api.StateRunning || !mc.Reachable() {
			return nil, http.StatusConflict, fmt.Errorf("%s is %s", mc.Name, mc.State)
		}
		d := net.Dialer{Timeout: 2 * time.Second}
		c, err := d.DialContext(ctx, "tcp", mc.Addr(port))
		if err == nil {
			return c, 0, nil
		}
		ultimo = err
		if ctx.Err() != nil {
			return nil, http.StatusBadGateway, ctx.Err()
		}
		if time.Now().After(limite) {
			return nil, http.StatusBadGateway, fmt.Errorf("nothing answers on port %d of %s: %w", port, mc.Name, ultimo)
		}
		time.Sleep(paso)
		paso = min(paso*2, 100*time.Millisecond)
	}
}

// relayTCP copia en los dos sentidos hasta que los dos acaban, uno falla al
// escribir o pasa idle sin bytes. cli lee del cliente (puede llevar un prefijo
// ya bufereado); conn es la conexión del cliente. Un EOF en un sentido se
// propaga como medio cierre si la conexión lo admite.
func relayTCP(ctx context.Context, conn net.Conn, cli io.Reader, guest net.Conn, idle time.Duration) {
	var wg sync.WaitGroup
	var ultima atomic.Int64 // última vez que pasó un byte, en cualquier sentido
	ultima.Store(time.Now().UnixNano())
	cerrar := func() { _ = conn.Close(); _ = guest.Close() }
	copiar := func(dst net.Conn, src io.Reader, srcConn net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			if idle > 0 {
				_ = srcConn.SetReadDeadline(time.Now().Add(idle))
			}
			n, err := src.Read(buf)
			var ne net.Error
			if err != nil && idle > 0 && errors.As(err, &ne) && ne.Timeout() &&
				time.Since(time.Unix(0, ultima.Load())) < idle {
				continue // este sentido calla, pero el otro no
			}
			if n > 0 {
				ultima.Store(time.Now().UnixNano())
				if idle > 0 {
					_ = dst.SetWriteDeadline(time.Now().Add(idle))
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					cerrar()
					return
				}
			}
			if err != nil {
				if cw, ok := dst.(closeWriter); ok && errors.Is(err, io.EOF) {
					_ = cw.CloseWrite()
				} else {
					cerrar()
				}
				return
			}
		}
	}
	wg.Add(2)
	go copiar(guest, cli, conn)
	go copiar(conn, guest, guest)
	fin := make(chan struct{})
	go func() { wg.Wait(); close(fin) }()
	select {
	case <-fin:
	case <-ctx.Done():
		cerrar()
		<-fin
	}
}

type closeWriter interface{ CloseWrite() error }
