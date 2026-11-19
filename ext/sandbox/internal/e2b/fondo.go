package e2b

// Procesos de fondo: Process/Start, Connect, List y SendSignal contra
// /v1/sandboxes/{id}/processes del gateway. El comando pertenece al sandbox, no
// a la conexión: si el cliente cierra el stream, sigue corriendo y se puede
// volver a él con Connect (como en el envd real).
//
// Si el gateway no conoce esas rutas (uno anterior), Start cae al exec atado a
// la petición y List/SendSignal solo ven lo que lanzó el propio shim.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// infoFondo es lo que el shim lee de api.ProcessInfo.
type infoFondo struct {
	PID      int      `json:"pid"`
	Cmd      []string `json:"cmd"`
	Dir      string   `json:"dir"`
	State    string   `json:"state"`
	ExitCode *int     `json:"exit_code"`
	Signal   string   `json:"signal"`
	TimedOut bool     `json:"timed_out"`
}

// eventoFondo es una línea de .../output (api.ProcessEvent).
type eventoFondo struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
	Offset int64  `json:"offset"`
	Exit   *int   `json:"exit"`
	Error  string `json:"error"`
}

func rutaProc(id string, pid int, sub string) string {
	r := rutaSB(id) + "/processes"
	if pid > 0 {
		r += "/" + strconv.Itoa(pid)
	}
	return r + sub
}

func claveProc(sandbox, tag string) string { return sandbox + "\x00" + tag }

// sinSoporte dice si el gateway no tiene procesos de fondo (404 de ruta, 405 o
// 501), en cuyo caso se usa el camino antiguo.
func sinSoporte(err error) bool {
	var g *errGW
	if !errors.As(err, &g) {
		return false
	}
	return g.Status == http.StatusNotFound || g.Status == http.StatusMethodNotAllowed || g.Status == http.StatusNotImplemented
}

// arrancarFondo lanza el comando como proceso de fondo y vuelca su salida en fl.
// Devuelve false (sin haber escrito nada) si hay que usar el exec antiguo.
func (s *Servidor) arrancarFondo(r *http.Request, fl *flujo, id string, cfg procConfig, tag string, cuerpo map[string]any, ms int) bool {
	var info infoFondo
	// Sin timeout_seconds: el plazo de E2B es el de la conexión, no la vida del
	// comando (que sigue si la conexión se cierra o caduca).
	if err := s.llamarGW(r.Context(), "POST", rutaProc(id, 0, ""), cuerpo, &info); err != nil {
		if sinSoporte(err) {
			return false
		}
		fl.fin(s.errConnectGW(err))
		return true
	}
	if tag != "" {
		s.mu.Lock()
		s.etiquetas[claveProc(id, tag)] = info.PID
		s.mu.Unlock()
	}
	s.volcarFondo(r, fl, id, info.PID, ms)
	return true
}

// volcarFondo manda "start", sigue la salida del proceso desde el principio y
// acaba con "end" cuando termina. Si el cliente se va, el proceso no se toca.
func (s *Servidor) volcarFondo(r *http.Request, fl *flujo, id string, pid, ms int) {
	ctx := r.Context()
	if ms > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
		defer cancel()
	}
	evento := func(k string, v any) error { return fl.mensaje(map[string]any{"event": map[string]any{k: v}}) }
	if evento("start", map[string]any{"pid": pid}) != nil {
		return
	}
	resp, err := s.gw(ctx, "GET", rutaProc(id, pid, "/output")+"?follow=1&offset=0", nil)
	if err != nil {
		fl.fin(&errConnect{cNoDisponible, err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fl.fin(s.errConnectGW(s.leerError(resp)))
		return
	}
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		linea, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(linea))) > 0 {
			var ev eventoFondo
			if json.Unmarshal(linea, &ev) != nil {
				fl.fin(&errConnect{cInterno, "invalid event from the gateway"})
				return
			}
			switch {
			case ev.Error != "":
				fl.fin(&errConnect{cNoDisponible, s.redactar(ev.Error)})
				return
			case ev.Exit != nil:
				s.finFondo(fl, id, pid, *ev.Exit)
				return
			case ev.Stream == "stdout" || ev.Stream == "stderr":
				if evento("data", map[string]any{ev.Stream: ev.Data}) != nil {
					return
				}
			}
		}
		if err != nil {
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				// Caducó la conexión, no el proceso.
				fl.fin(&errConnect{cPlazo, "the command connection timed out; the process keeps running"})
			case r.Context().Err() == nil:
				fl.fin(&errConnect{cNoDisponible, "the process output ended without an exit event"})
			}
			return
		}
	}
}

// finFondo manda "end" con el estado final y olvida el proceso en el sandbox
// (ya se entregó su resultado, como hace envd).
func (s *Servidor) finFondo(fl *flujo, id string, pid, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var info infoFondo
	_ = s.llamarGW(ctx, "GET", rutaProc(id, pid, ""), nil, &info) // la señal y el plazo no salen del flujo
	fin := map[string]any{"exitCode": code, "exited": true, "status": "exit status " + strconv.Itoa(code)}
	switch {
	case info.Signal != "":
		fin["status"] = "signal: " + sigNombreFondo(info.Signal)
	case info.TimedOut:
		fin["error"] = "command timed out"
	}
	_ = fl.mensaje(map[string]any{"event": map[string]any{"end": fin}})
	fl.fin(nil)
	_ = s.llamarGW(ctx, "DELETE", rutaProc(id, pid, ""), nil, nil)
	s.mu.Lock()
	for k, v := range s.etiquetas {
		if v == pid && strings.HasPrefix(k, id+"\x00") {
			delete(s.etiquetas, k)
		}
	}
	s.mu.Unlock()
}

func sigNombreFondo(n string) string {
	if strings.TrimPrefix(strings.ToUpper(n), "SIG") == "KILL" {
		return "killed"
	}
	return "terminated"
}

// pidFondo resuelve un selector (pid o tag) de E2B al pid del sandbox,
// comprobando que el proceso existe en el gateway.
func (s *Servidor) pidFondo(ctx context.Context, id string, sel selector) (int, error) {
	pid := 0
	switch {
	case sel.PID != nil:
		pid = int(*sel.PID)
	case sel.Tag != nil:
		s.mu.Lock()
		pid = s.etiquetas[claveProc(id, *sel.Tag)]
		s.mu.Unlock()
	}
	if pid <= 0 {
		return 0, &errGW{Status: http.StatusNotFound, Msg: "process not found"}
	}
	var info infoFondo
	if err := s.llamarGW(ctx, "GET", rutaProc(id, pid, ""), nil, &info); err != nil {
		if sinSoporte(err) {
			return 0, &errGW{Status: http.StatusNotFound, Msg: "process not found"}
		}
		return 0, err
	}
	return pid, nil
}

// procConnect (Process/Connect): vuelve a un proceso ya lanzado y sigue su
// salida desde el principio.
func (s *Servidor) procConnect(w http.ResponseWriter, r *http.Request, id string) {
	if tipoContenido(r) != tipoStreaming {
		rechazarTipo(w, r, tipoStreaming)
		return
	}
	fl := nuevoFlujo(w)
	_, carga, err := leerSobre(r.Body)
	if err != nil {
		fl.fin(&errConnect{cInvalido, err.Error()})
		return
	}
	var req struct {
		Process selector `json:"process"`
	}
	if err := json.Unmarshal(carga, &req); err != nil {
		fl.fin(&errConnect{cInvalido, "invalid JSON request: " + err.Error()})
		return
	}
	pid, err := s.pidFondo(r.Context(), id, req.Process)
	if err != nil {
		fl.fin(s.errConnectGW(err))
		return
	}
	ms, _ := strconv.Atoi(r.Header.Get("Connect-Timeout-Ms"))
	s.volcarFondo(r, fl, id, pid, ms)
}

// listarFondo da los procesos en marcha del sandbox según el gateway, con la
// forma de Process/List.
func (s *Servidor) listarFondo(ctx context.Context, id string) []map[string]any {
	var ps []infoFondo
	if s.llamarGW(ctx, "GET", rutaProc(id, 0, ""), nil, &ps) != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, p := range ps {
		if p.State != "running" || len(p.Cmd) == 0 {
			continue
		}
		c0, a0 := desenvolverBash(p.Cmd)
		cfg := procConfig{Cmd: c0, Args: a0}
		if p.Dir != "" {
			cfg.Cwd = &p.Dir
		}
		e := map[string]any{"config": cfg, "pid": p.PID}
		for k, v := range s.etiquetas {
			if v == p.PID && strings.HasPrefix(k, id+"\x00") {
				e["tag"] = strings.TrimPrefix(k, id+"\x00")
			}
		}
		out = append(out, e)
	}
	return out
}

// senalFondo atiende SendSignal para un proceso que no es del camino antiguo.
func (s *Servidor) senalFondo(w http.ResponseWriter, r *http.Request, id string, sel selector, sg int32) {
	pid, err := s.pidFondo(r.Context(), id, sel)
	if err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	nombre := "TERM"
	if sg == 9 {
		nombre = "KILL"
	}
	if err := s.llamarGW(r.Context(), "POST", rutaProc(id, pid, "/signal"), map[string]string{"signal": nombre}, nil); err != nil {
		escribirErrorUnario(w, s.errConnectGW(err))
		return
	}
	escribirJSON(w, 200, map[string]any{})
}
