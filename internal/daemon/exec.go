package daemon

// Exec, ficheros y sandboxes: lo que el daemon hace es comprobar que la máquina
// puede recibirlos, hablar con el agente del invitado —que solo es alcanzable
// desde aquí— y pasar la respuesta en streaming, sin fiarse de lo que el
// invitado mande: el código de dentro se considera hostil, y el agente corre
// dentro.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// execStatus traduce los errores del manager a códigos HTTP.
func execStatus(err error) int {
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		return http.StatusNotFound
	case errors.Is(err, machine.ErrExecNotAllowed):
		return http.StatusForbidden
	case errors.Is(err, machine.ErrNotRunning), errors.Is(err, machine.ErrExecNotInSnapshot):
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

func guestBase(mc *api.Machine) string {
	return "http://" + net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort))
}

// agentWait es cuánto se espera a que el agente del invitado escuche. Una
// máquina recién arrancada en frío tarda unos segundos en tenerlo: `kling run`
// vuelve cuando el VMM arranca, no cuando el invitado está listo, y un exec justo
// detrás no debe fallar por eso. Si ya escucha, no cuesta nada.
const agentWait = 30 * time.Second

func waitAgent(ctx context.Context, mc *api.Machine) error {
	if err := waitPort(ctx, net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort)), agentWait); err != nil {
		return fmt.Errorf("the guest agent of %s is not listening: %w", mc.Name, err)
	}
	return nil
}

// tooOldAgent es la respuesta cuando el agente del invitado no conoce la ruta:
// la imagen se construyó antes de v0.7.
func tooOldAgent(mc *api.Machine) error {
	return fmt.Errorf("the guest agent in image %q predates exec streaming and files (kindling v0.7); "+
		"rebuild the image (kling images build -builder base, or kling images toolchain)", mc.Image)
}

// handleExec sirve POST /machines/{ref}/exec: NDJSON de api.ExecEvent, o con
// ?wait=1 un api.ExecResult al terminar.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req api.ExecRequest
	// El stdin viaja en base64 (4/3 del tamaño) y con su sobre JSON.
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*api.ExecMaxStdin+64<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	_, maxOut, err := req.Limits()
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mc, err := s.mgr.ExecTarget(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, execStatus(err), err)
		return
	}

	if err := waitAgent(r.Context(), mc); err != nil {
		fail(w, http.StatusGatewayTimeout, err)
		return
	}
	body, code, err := openGuestExec(r.Context(), guestBase(mc), req)
	if err != nil {
		if code == http.StatusNotImplemented {
			err = tooOldAgent(mc)
		}
		fail(w, code, err)
		return
	}
	defer body.Close()

	events := readExecEvents(body, maxOut)
	if r.URL.Query().Get("wait") == "1" {
		res, err := aggregateExec(events)
		if err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for ev := range events {
		if enc.Encode(ev) != nil {
			// Quien llamaba se fue. Al volver, el contexto de la petición se
			// cancela, se corta la conexión con el invitado y el agente mata el
			// comando. El lector aún puede tener eventos que mandar: se vacía el
			// canal para que no se quede bloqueado para siempre.
			go func() {
				for range events {
				}
			}()
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// openGuestExec pide al agente del invitado en base que ejecute req y devuelve
// su flujo NDJSON. Si falla, code es el estado HTTP con el que contestar (501:
// el agente no conoce la ruta).
func openGuestExec(ctx context.Context, base string, req api.ExecRequest) (io.ReadCloser, int, error) {
	body, _ := json.Marshal(req)
	greq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/exec/stream", bytes.NewReader(body))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	greq.Header.Set("Content-Type", "application/json")
	resp, err := guestClient.Do(greq)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("talking to the guest agent: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, 0, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, http.StatusNotImplemented, errors.New("the guest agent has no exec streaming")
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	code := http.StatusBadGateway
	if resp.StatusCode == http.StatusBadRequest {
		code = http.StatusBadRequest
	}
	return nil, code, fmt.Errorf("guest agent: %s", strings.TrimSpace(string(msg)))
}

// readExecEvents lee el flujo del invitado y lo pasa como eventos ya validados.
// Garantiza lo que el invitado podría no cumplir: que ningún flujo pase de
// maxOut, que los flujos se llamen stdout o stderr, y que haya exactamente un
// evento final (Exit o Error), que es siempre el último.
func readExecEvents(body io.Reader, maxOut int) <-chan api.ExecEvent {
	ch := make(chan api.ExecEvent, 16)
	go func() {
		defer close(ch)
		left := map[string]int{"stdout": maxOut, "stderr": maxOut}
		truncated := false
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var ev api.ExecEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				ch <- api.ExecEvent{Error: fmt.Sprintf("the guest agent sent an unreadable event: %v", err)}
				return
			}
			switch {
			case ev.Error != "":
				ch <- api.ExecEvent{Error: ev.Error}
				return
			case ev.Exit != nil:
				ch <- api.ExecEvent{Exit: ev.Exit, DurationMS: ev.DurationMS,
					Truncated: ev.Truncated || truncated, TimedOut: ev.TimedOut}
				return
			case ev.Stream == "stdout" || ev.Stream == "stderr":
				n := left[ev.Stream]
				if len(ev.Data) > n {
					ev.Data = ev.Data[:n]
					truncated = true
				}
				left[ev.Stream] = n - len(ev.Data)
				if len(ev.Data) > 0 {
					ch <- api.ExecEvent{Stream: ev.Stream, Data: ev.Data}
				}
			}
		}
		msg := "the guest stopped answering before the command finished"
		if err := sc.Err(); err != nil {
			msg += ": " + err.Error()
		}
		ch <- api.ExecEvent{Error: msg}
	}()
	return ch
}

// aggregateExec junta el flujo en un resultado (?wait=1).
func aggregateExec(events <-chan api.ExecEvent) (*api.ExecResult, error) {
	var stdout, stderr bytes.Buffer
	for ev := range events {
		switch {
		case ev.Error != "":
			for range events {
			}
			return nil, errors.New(ev.Error)
		case ev.Exit != nil:
			for range events {
			}
			return &api.ExecResult{ExitCode: *ev.Exit, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
				DurationMS: ev.DurationMS, Truncated: ev.Truncated, TimedOut: ev.TimedOut}, nil
		case ev.Stream == "stderr":
			stderr.Write(ev.Data)
		default:
			stdout.Write(ev.Data)
		}
	}
	return nil, errors.New("the exec stream ended without an exit code")
}

// handleFiles sirve GET/PUT/DELETE /machines/{ref}/files?path=...
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	for _, k := range []string{"path", "stat", "mode", "mkdir"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	if q.Get("path") == "" {
		fail(w, http.StatusBadRequest, errors.New("missing path"))
		return
	}
	var body io.Reader
	if r.Method == http.MethodPut {
		if r.ContentLength > api.FileMaxUpload {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("upload is %d bytes; the limit is %d", r.ContentLength, api.FileMaxUpload))
			return
		}
		// El plazo de lectura del servidor (30 s) es para peticiones JSON; subir
		// 64 MiB por un túnel SSH lento puede tardar más.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Minute))
		body = io.LimitReader(r.Body, api.FileMaxUpload+1)
	}

	mc, err := s.mgr.ExecTarget(r.Context(), r.PathValue("ref"))
	if err != nil {
		fail(w, execStatus(err), err)
		return
	}
	if err := waitAgent(r.Context(), mc); err != nil {
		fail(w, http.StatusGatewayTimeout, err)
		return
	}
	greq, err := http.NewRequestWithContext(r.Context(), r.Method, guestBase(mc)+"/files?"+q.Encode(), body)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if r.ContentLength > 0 {
		greq.ContentLength = r.ContentLength
	}
	resp, err := guestClient.Do(greq)
	if err != nil {
		fail(w, http.StatusBadGateway, fmt.Errorf("talking to the guest agent: %w", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		text := strings.TrimSpace(string(msg))
		// Un 404 sin cuerpo propio del agente es que no conoce la ruta.
		if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(text, "404 page not found") {
			fail(w, http.StatusNotImplemented, tooOldAgent(mc))
			return
		}
		code := resp.StatusCode
		if code >= 500 {
			code = http.StatusBadGateway
		}
		fail(w, code, errors.New(text))
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(resp.StatusCode)
	// Tope también aquí: el invitado dice lo que quiera en Content-Length.
	_, _ = io.Copy(w, io.LimitReader(resp.Body, api.FileMaxDownload))
}

// handleCreateSandbox sirve POST /sandboxes.
func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req api.SandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = api.SandboxDefaultTTL
	}
	if ttl < 0 || ttl > api.SandboxMaxTTL {
		fail(w, http.StatusBadRequest, fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL))
		return
	}
	if req.Image != "" && req.From != "" {
		fail(w, http.StatusBadRequest, errors.New("pass image or from, not both"))
		return
	}
	if req.Image == "" && req.From == "" {
		fail(w, http.StatusBadRequest, errors.New("missing image (or from, a snapshot made with allow_exec). "+
			"Any image with the guest agent works: kling images toolchain builds one with node and python"))
		return
	}
	onTTL := req.OnTTL
	switch onTTL {
	case "", api.OnTTLRemove:
		onTTL = api.OnTTLRemove
	case api.OnTTLFreeze:
	default:
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid on_ttl %q: use %q or %q", req.OnTTL, api.OnTTLRemove, api.OnTTLFreeze))
		return
	}
	egress := req.Egress
	if egress == "" {
		// Sin red por defecto: lo que corre en un sandbox lo escribió un agente, y
		// no se le da salida a nada que no se haya pedido.
		egress = "none"
	}
	if req.Image != "" {
		has, err := s.mgr.ImageHasAgent(r.Context(), req.Image)
		if err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("image %q: %w", req.Image, err))
			return
		}
		if !has {
			fail(w, http.StatusBadRequest, fmt.Errorf("image %q has no guest agent, so nobody inside would run commands; "+
				"build one with kling images build -builder base (or kling images toolchain)", req.Image))
			return
		}
	}

	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	labels[api.LabelKind] = api.KindSandbox

	mc, err := s.mgr.Run(r.Context(), api.RunRequest{
		Name: req.Name, Image: req.Image, From: req.From,
		VCPUs: req.VCPUs, MemMiB: req.MemMiB, CPUPct: req.CPUPct,
		Egress: egress, AllowDomains: req.AllowDomains,
		TTLSeconds: ttl, OnTTL: onTTL,
		Volumes: req.Volumes, Labels: labels,
		AllowExec: true,
	})
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, machine.ErrExecNotInSnapshot) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	// Se devuelve cuando el agente ya escucha: quien crea un sandbox va a
	// ejecutar algo acto seguido, y un "connection refused" en la primera
	// llamada sería culpa nuestra.
	if err := waitPort(r.Context(), net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort)), 60*time.Second); err != nil {
		_ = s.mgr.Remove(mc.ID)
		fail(w, http.StatusGatewayTimeout, fmt.Errorf("the sandbox booted but its guest agent never listened: %w", err))
		return
	}
	writeJSON(w, http.StatusCreated, mc)
}

// sandbox resuelve ref a una máquina que sea un sandbox; las demás no se tocan
// por estas rutas.
func (s *Server) sandbox(w http.ResponseWriter, ref string) (*api.Machine, bool) {
	mc, ok := s.mgr.Get(ref)
	if !ok || mc.Labels[api.LabelKind] != api.KindSandbox {
		fail(w, http.StatusNotFound, fmt.Errorf("no sandbox %q", ref))
		return nil, false
	}
	return mc, true
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	out := []*api.Machine{}
	for _, mc := range s.mgr.List() {
		if mc.Labels[api.LabelKind] == api.KindSandbox {
			out = append(out, mc)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	if mc, ok := s.sandbox(w, r.PathValue("ref")); ok {
		writeJSON(w, http.StatusOK, mc)
	}
}

func (s *Server) handleRenewSandbox(w http.ResponseWriter, r *http.Request) {
	mc, ok := s.sandbox(w, r.PathValue("ref"))
	if !ok {
		return
	}
	var req api.RenewRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = api.SandboxDefaultTTL
	}
	if ttl < 0 || ttl > api.SandboxMaxTTL {
		fail(w, http.StatusBadRequest, fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL))
		return
	}
	out, err := s.mgr.Renew(mc.ID, ttl)
	if err != nil {
		fail(w, execStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRemoveSandbox(w http.ResponseWriter, r *http.Request) {
	mc, ok := s.sandbox(w, r.PathValue("ref"))
	if !ok {
		return
	}
	if err := s.mgr.Remove(mc.ID); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
