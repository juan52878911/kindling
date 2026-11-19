package daemon

// Procesos en segundo plano y sesiones (docs/exec-sandbox.md): el daemon solo
// valida la ruta, despierta la máquina si hace falta (ExecTarget) y reenvía al
// agente del invitado, que es quien guarda la tabla y la salida. Por eso un
// freeze y thaw los conserva: viven en la memoria del invitado.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// maxCuerpoProc acota lo que se reenvía al agente: son cuerpos pequeños
// (comando, entorno, señal).
const maxCuerpoProc = 1 << 20

// maxEsperaProc es el mayor ?wait= que se acepta de una vez (el cliente puede
// repetir).
const maxEsperaProc = 10 * time.Minute

// rutaInvitado es la parte de la ruta del daemon que va al agente: lo que
// sigue a /machines/{ref}, con su escape.
func rutaInvitado(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/machines/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[i:]
	}
	return "/"
}

func (s *Server) handleProcesos(w http.ResponseWriter, r *http.Request) {
	if pid := r.PathValue("pid"); pid != "" {
		if n, err := strconv.Atoi(pid); err != nil || n <= 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("invalid pid %q", pid))
			return
		}
	}
	s.proxyInvitado(w, r)
}

func (s *Server) proxyInvitado(w http.ResponseWriter, r *http.Request) {
	soltar, ok := s.tomarSesionGuest(w, r)
	if !ok {
		return
	}
	defer soltar()

	mc, code, err := s.objetivoConAgente(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, code, err)
		return
	}
	s.reenviar(w, r, guestBase(mc), mc.ID, mc)
}

// reenviar manda la petición al agente en base y devuelve su respuesta tal
// cual (en flujo). mc solo sirve para el mensaje de un agente antiguo.
func (s *Server) reenviar(w http.ResponseWriter, r *http.Request, base, id string, mc *api.Machine) {
	q := r.URL.Query()
	espera := time.Duration(0)
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("invalid wait %q", v))
			return
		}
		espera = time.Duration(n) * time.Second
		if espera > maxEsperaProc {
			espera = maxEsperaProc
			q.Set("wait", strconv.Itoa(int(espera/time.Second)))
		}
	}
	var body io.Reader
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, maxCuerpoProc)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			fail(w, jsonBodyStatus(err), fmt.Errorf("invalid body: %w", err))
			return
		}
		body = strings.NewReader(string(b))
	}

	ctx := r.Context()
	if espera > 0 {
		// El agente no manda cabeceras hasta que acaba la espera.
		ctx = conPlazoCabeceras(ctx, espera+execBodyGrace)
	}
	url := base + rutaInvitado(r)
	if enc := q.Encode(); enc != "" {
		url += "?" + enc
	}
	greq, err := http.NewRequestWithContext(ctx, r.Method, url, body)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if body != nil {
		greq.Header.Set("Content-Type", "application/json")
	}
	resp, err := guestClient.Do(greq)
	if err != nil {
		s.olvidarAgente(id)
		code := http.StatusBadGateway
		var e error = fmt.Errorf("talking to the guest agent: %w", err)
		if x := s.execInterrumpido(id); x != nil {
			code, e = http.StatusConflict, x
		}
		fail(w, code, e)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		text := strings.TrimSpace(string(msg))
		// Un 404 con el texto del mux del agente es que no conoce la ruta
		// (agente anterior a los procesos de fondo); uno con otro texto es "no
		// existe ese pid o sesión".
		if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(text, "404 page not found") {
			fail(w, http.StatusNotImplemented, tooOldAgent(mc))
			return
		}
		code := resp.StatusCode
		if code >= 500 {
			code = http.StatusBadGateway
		}
		var e error = errors.New(text)
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", guestBusyRetryAfter)
		}
		fail(w, code, e)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	copiarFlujo(ctx, w, wrapGuestBody(resp.Body))
}

// copiarFlujo copia src a w empujando cada trozo (la salida en vivo de un
// ?follow=1 no puede quedarse en un búfer del servidor).
func copiarFlujo(ctx context.Context, w http.ResponseWriter, src io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}
