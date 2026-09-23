package daemon

// El relé de la shell interactiva.
//
// Las IP de los invitados solo existen en la red del host, así que una shell
// dentro de una microVM tiene que pasar por aquí. El daemon habla el mismo
// protocolo por los dos lados (ver pkg/api/shell.go): recibe un Upgrade, abre
// otro contra el agente del invitado y se queda en medio pasando tramas.
//
// En medio, no de paso: el invitado se considera hostil, así que cada trama se
// decodifica, se comprueba que su tipo tiene sentido en esa dirección y se
// vuelve a escribir. Reenviar bytes a ciegas dejaría que el invitado mandase al
// cliente tramas de redimensionado o cargas de 4 GiB.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// shellPing es cada cuánto se manda una trama vacía al cliente. Detecta al que
// se fue sin cerrar (un SSH muerto) sin depender de que el invitado hable.
const shellPing = 30 * time.Second

func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), api.ShellProto) {
		fail(w, http.StatusUpgradeRequired, fmt.Errorf("this endpoint speaks %s; ask for it with Upgrade", api.ShellProto))
		return
	}
	var req api.ShellRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	mc, err := s.mgr.ExecTarget(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, execStatus(err), err)
		return
	}
	if err := s.waitAgent(r.Context(), mc); err != nil {
		fail(w, http.StatusGatewayTimeout, err)
		return
	}

	// Primero el invitado y después el secuestro: mientras la respuesta siga
	// siendo HTTP normal, un fallo se cuenta con un código y un mensaje. Después
	// del 101 ya no hay forma de decir nada que el cliente entienda como error.
	guest, err := openGuestShell(r.Context(), guestBase(mc), req)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, api.ErrShellUnsupported) {
			code, err = http.StatusNotImplemented, tooOldAgent(mc)
		}
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
		api.ShellProto + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}
	relayShell(r.Context(), brw, conn, guest)
}

// openGuestShell abre la sesión contra el agente del invitado.
func openGuestShell(ctx context.Context, base string, req api.ShellRequest) (io.ReadWriteCloser, error) {
	body, _ := json.Marshal(req)
	greq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/exec/pty", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	greq.Header.Set("Content-Type", "application/json")
	greq.Header.Set("Connection", "Upgrade")
	greq.Header.Set("Upgrade", api.ShellProto)

	resp, err := guestClient.Do(greq)
	if err != nil {
		return nil, fmt.Errorf("talking to the guest agent: %w", err)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		rwc, ok := resp.Body.(io.ReadWriteCloser)
		if !ok {
			resp.Body.Close()
			return nil, errors.New("the guest connection could not be taken over")
		}
		return rwc, nil
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusUpgradeRequired, http.StatusNotImplemented:
		return nil, fmt.Errorf("%w: %s", api.ErrShellUnsupported, strings.TrimSpace(string(msg)))
	}
	return nil, fmt.Errorf("guest agent: %s", strings.TrimSpace(string(msg)))
}

// relayShell pasa tramas entre el cliente y el invitado hasta que uno cuelga.
func relayShell(ctx context.Context, fromClient io.Reader, toClient io.Writer, guest io.ReadWriteCloser) {
	var mu sync.Mutex
	alCliente := func(t byte, p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return api.WriteFrame(toClient, t, p)
	}

	fin := make(chan struct{})
	var once sync.Once
	cerrar := func() { once.Do(func() { close(fin) }) }

	// Invitado → cliente. Solo salida, final y error: el invitado no tiene por
	// qué mandar redimensionados ni señales, y aceptárselos sería darle un canal
	// hacia el terminal de quien llama.
	go func() {
		defer cerrar()
		for {
			t, p, err := api.ReadFrame(guest)
			if err != nil {
				_ = alCliente(api.ShellError, []byte("the guest stopped answering"))
				return
			}
			switch t {
			case api.ShellData, api.ShellExit, api.ShellError:
				if err := alCliente(t, p); err != nil {
					return
				}
				if t != api.ShellData {
					return
				}
			default:
				_ = alCliente(api.ShellError, []byte(fmt.Sprintf("the guest sent a frame of type %d, which it must not", t)))
				return
			}
		}
	}()

	// Cliente → invitado. Aquí al revés: nada de exit ni de error, que son del
	// invitado.
	go func() {
		defer cerrar()
		for {
			t, p, err := api.ReadFrame(fromClient)
			if err != nil {
				return
			}
			switch t {
			case api.ShellData, api.ShellResize, api.ShellSignal:
				if err := api.WriteFrame(guest, t, p); err != nil {
					return
				}
			default:
				// Una trama que no viene a cuento no tumba la sesión: se ignora.
			}
		}
	}()

	tick := time.NewTicker(shellPing)
	defer tick.Stop()
	for {
		select {
		case <-fin:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := alCliente(api.ShellPing, nil); err != nil {
				return
			}
		}
	}
}
