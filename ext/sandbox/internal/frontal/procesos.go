package frontal

// Procesos en segundo plano y sesiones: passthrough al daemon con la misma
// comprobación de dueño que exec y files (buscarEjecutable resuelve el sandbox
// DEL tenant o contesta 404) y topes por tenant.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// maxProcesosPorDefecto y maxSesionesPorDefecto acotan los procesos de
	// fondo vivos y las sesiones que un sandbox puede tener cuando el tenant no
	// trae su tope (Tenant.MaxProcesos / MaxSesiones).
	maxProcesosPorDefecto = 32
	maxSesionesPorDefecto = 16
	// maxSeguirPorDefecto: lecturas de salida con follow abiertas a la vez por
	// tenant (Tenant.MaxSeguir). Cada una es una conexión que el frontal
	// mantiene en pie.
	maxSeguirPorDefecto = 32
	// maxEsperaProceso acota ?wait= de GET processes/{pid}.
	maxEsperaProceso = 300
)

func topeONumero(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// despachaProcesos atiende /v1/sandboxes/{id}/processes[/{pid}[/output|/signal]].
// resto son los segmentos tras "processes".
func (s *Servidor) despachaProcesos(w http.ResponseWriter, r *http.Request, id string, resto []string) {
	switch len(resto) {
	case 0:
		switch r.Method {
		case http.MethodPost:
			s.handleProcesoNuevo(w, r, id)
		case http.MethodGet:
			s.handleProcesos(w, r, id)
		default:
			fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
		}
		return
	case 1, 2:
	default:
		noRuta(w, r)
		return
	}
	pid, err := strconv.Atoi(resto[0])
	if err != nil || pid <= 0 {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid pid %q", resto[0]))
		return
	}
	sub := ""
	if len(resto) == 2 {
		sub = resto[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.handleProceso(w, r, id, pid)
	case sub == "" && r.Method == http.MethodDelete:
		s.handleProcesoBorrar(w, r, id, pid)
	case sub == "output" && r.Method == http.MethodGet:
		s.handleProcesoSalida(w, r, id, pid)
	case sub == "signal" && r.Method == http.MethodPost:
		s.handleProcesoSenal(w, r, id, pid)
	case sub == "" || sub == "output" || sub == "signal":
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	default:
		noRuta(w, r)
	}
}

func (s *Servidor) handleProcesoNuevo(w http.ResponseWriter, r *http.Request, id string) {
	var req api.ProcessStartRequest
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Cmd) == 0 {
		fail(w, http.StatusBadRequest, errors.New("cmd is required"))
		return
	}
	t := tenantDe(r)
	h, mc, err := s.buscarEjecutable(r.Context(), t, id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	porDefecto := req.Session == "" && sesionPorDefecto(mc) != ""
	if porDefecto {
		req.Session = idSesionPorDefecto
	}
	// Tope de procesos vivos por sandbox. Se cuenta preguntando al daemon:
	// los que ya terminaron no gastan cuota.
	tope := topeONumero(t.MaxProcesos, maxProcesosPorDefecto)
	ps, err := h.Cliente.Processes(r.Context(), mc.ID)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	vivos := 0
	for _, p := range ps {
		if p.State == api.ProcessRunning {
			vivos++
		}
	}
	if vivos >= tope {
		fail(w, http.StatusTooManyRequests, fmt.Errorf(
			"sandbox %s already has %d background process(es) running; the limit is %d", id, vivos, tope))
		return
	}
	p, err := h.Cliente.StartProcess(r.Context(), mc.ID, req)
	if err != nil && porDefecto && esSesionPerdida(err) {
		req.Session = ""
		p, err = h.Cliente.StartProcess(r.Context(), mc.ID, req)
	}
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Servidor) handleProcesos(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	ps, err := h.Cliente.Processes(r.Context(), mc.ID)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if ps == nil {
		ps = []api.ProcessInfo{}
	}
	writeJSON(w, http.StatusOK, ps)
}

func (s *Servidor) handleProceso(w http.ResponseWriter, r *http.Request, id string, pid int) {
	var espera time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("invalid wait %q", v))
			return
		}
		if n > maxEsperaProceso {
			n = maxEsperaProceso
		}
		espera = time.Duration(n) * time.Second
	}
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	p, err := h.Cliente.Process(r.Context(), mc.ID, pid, espera)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Servidor) handleProcesoBorrar(w http.ResponseWriter, r *http.Request, id string, pid int) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	if err := h.Cliente.RemoveProcess(r.Context(), mc.ID, pid); err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Servidor) handleProcesoSenal(w http.ResponseWriter, r *http.Request, id string, pid int) {
	var req api.ProcessSignalRequest
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	p, err := h.Cliente.SignalProcess(r.Context(), mc.ID, pid, req.Signal)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleProcesoSalida sirve GET .../output?offset=N&follow=1: NDJSON de
// api.ProcessEvent tal como lo da el daemon.
func (s *Servidor) handleProcesoSalida(w http.ResponseWriter, r *http.Request, id string, pid int) {
	q := r.URL.Query()
	var offset int64
	if v := q.Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("invalid offset %q", v))
			return
		}
		offset = n
	}
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"
	t := tenantDe(r)
	h, mc, err := s.buscarEjecutable(r.Context(), t, id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	if follow {
		if !s.seguir.abrir(t.Nombre, topeONumero(t.MaxSeguir, maxSeguirPorDefecto)) {
			fail(w, http.StatusTooManyRequests, fmt.Errorf(
				"tenant %s already follows %d process output(s); the limit is %d",
				t.Nombre, s.seguir.cuenta(t.Nombre), topeONumero(t.MaxSeguir, maxSeguirPorDefecto)))
			return
		}
		defer s.seguir.cerrar(t.Nombre)
	}

	// Como en exec: las cabeceras salen con el primer evento, para poder
	// contestar un 404 del daemon con su código.
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	empezado := false
	emitir := func(ev api.ProcessEvent) error {
		if !empezado {
			empezado = true
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
		}
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	exit, next, err := h.Cliente.ProcessOutput(r.Context(), mc.ID, pid, offset, follow, emitir)
	switch {
	case err != nil && !empezado:
		fail(w, codigoExec(err), deHost(h, err))
	case err != nil:
		_ = emitir(api.ProcessEvent{Offset: next, Error: errorDeFlujo(deHost(h, err))})
	default:
		// Cierre explícito: el offset desde el que continuar y, si terminó, su código.
		_ = emitir(api.ProcessEvent{Offset: next, Exit: exit})
	}
}

// ---- sesiones

// despachaSesiones atiende /v1/sandboxes/{id}/sessions[/{sid}].
func (s *Servidor) despachaSesiones(w http.ResponseWriter, r *http.Request, id string, resto []string) {
	switch {
	case len(resto) == 0 && r.Method == http.MethodPost:
		s.handleSesionNueva(w, r, id)
	case len(resto) == 0 && r.Method == http.MethodGet:
		s.handleSesiones(w, r, id)
	case len(resto) == 1 && resto[0] != "" && r.Method == http.MethodGet:
		s.handleSesion(w, r, id, resto[0])
	case len(resto) == 1 && resto[0] != "" && r.Method == http.MethodDelete:
		s.handleSesionBorrar(w, r, id, resto[0])
	case len(resto) <= 1:
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	default:
		noRuta(w, r)
	}
}

func (s *Servidor) handleSesionNueva(w http.ResponseWriter, r *http.Request, id string) {
	var req api.SessionRequest
	if err := leerJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	t := tenantDe(r)
	h, mc, err := s.buscarEjecutable(r.Context(), t, id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	tope := topeONumero(t.MaxSesiones, maxSesionesPorDefecto)
	ss, err := h.Cliente.Sessions(r.Context(), mc.ID)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if len(ss) >= tope {
		fail(w, http.StatusTooManyRequests, fmt.Errorf(
			"sandbox %s already has %d session(s); the limit is %d", id, len(ss), tope))
		return
	}
	se, err := h.Cliente.CreateSession(r.Context(), mc.ID, req)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	writeJSON(w, http.StatusCreated, se)
}

func (s *Servidor) handleSesiones(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	ss, err := h.Cliente.Sessions(r.Context(), mc.ID)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	if ss == nil {
		ss = []api.Session{}
	}
	writeJSON(w, http.StatusOK, ss)
}

func (s *Servidor) handleSesion(w http.ResponseWriter, r *http.Request, id, sid string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	se, err := h.Cliente.GetSession(r.Context(), mc.ID, sid)
	if err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	writeJSON(w, http.StatusOK, se)
}

func (s *Servidor) handleSesionBorrar(w http.ResponseWriter, r *http.Request, id, sid string) {
	h, mc, err := s.buscarEjecutable(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	if err := h.Cliente.RemoveSession(r.Context(), mc.ID, sid); err != nil {
		fail(w, codigoExec(err), deHost(h, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
