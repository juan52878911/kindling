package guest

// GET /ready y POST /hooks: "listo" según la imagen y ganchos tras restaurar.
//
// La imagen declara en su rootfs una sonda (api.GuestReadyProbe) y ganchos
// (api.GuestPostRestoreDir); el agente los ejecuta él mismo. No dependen de
// kling.exec: lo que corre es código de la imagen, en rutas fijas y sin
// argumentos de quien llama, así que quien alcanza el puerto del agente puede
// como mucho pedir que se ejecute lo que la imagen ya iba a ejecutar.
//
// Decisiones:
//
//   - "Listo" se recuerda. Es "terminó de arrancar", no un chequeo de vida:
//     una vez que la sonda contesta 0 no se vuelve a ejecutar, y la memoria de
//     un dorado guardado ya listo trae el recuerdo a cada copia. Lo que cambia
//     tras restaurar lo hacen los ganchos, y mientras corren no está listo.
//   - Los ganchos los lanza el daemon (POST /hooks) al final de cada
//     restauración, cuando ya resincronizó el reloj, montó los volúmenes y
//     entregó las credenciales; /resync solo le dice si los hay. Corren en
//     SEGUNDO PLANO: quien necesite esperarlos pregunta /ready.
//   - Uno que falla (o agota su plazo) para la tanda y deja "failed": no se
//     reintenta solo. POST /hooks los vuelve a lanzar.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Plazos. La sonda se pregunta a menudo y tiene que ser barata: 10 s es de
// sobra para un `getprop`; una imagen que necesita más lo dice en su servicio
// (api.ServiceSpec.ReadyTimeoutSeconds: el Timeout de su HEALTHCHECK). Un gancho puede hacer trabajo de verdad (aplicar una
// identidad, regenerar claves), pero no puede dejar la máquina "no lista"
// indefinidamente.
const (
	readyProbeTimeout = 10 * time.Second
	hookTimeout       = 60 * time.Second
	// readyDetailMax acota la salida que se devuelve en Detail.
	readyDetailMax = 512
)

// readiness es el estado de la sonda y de los ganchos de este invitado.
type readiness struct {
	probePath string
	hooksDir  string

	// run ejecuta un programa con plazo y devuelve su salida recortada.
	// Sustituible en tests.
	run func(ctx context.Context, path string, env []string) (string, error)
	// probe ejecuta la sonda; nil = run. Por defecto, con el usuario del
	// servicio de la imagen (como el HEALTHCHECK de Docker); los ganchos, root.
	probe func(ctx context.Context, path string, env []string) (string, error)

	probeMu sync.Mutex // una sonda a la vez: dos /ready seguidos esperan a la misma
	mu      sync.Mutex // protege lo de abajo
	env     []string
	ok      bool // la sonda ya contestó 0 una vez
	detail  string
	hooks   string // api.Hooks*
	hookErr string
}

// readyState es el del proceso, como shareState.
var readyState = &readiness{
	probePath: api.GuestReadyProbe,
	hooksDir:  api.GuestPostRestoreDir,
	run:       runWithTimeout,
	probe:     runProbeAsService,
}

// setEnv fija el entorno de la sonda y los ganchos (el de los servicios).
func (r *readiness) setEnv(env []string) {
	r.mu.Lock()
	r.env = env
	r.mu.Unlock()
}

// hasProbe dice si la imagen declara sonda: un fichero regular ejecutable.
func (r *readiness) hasProbe() bool {
	fi, err := os.Stat(r.probePath)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// snapshot es el estado sin ejecutar nada: lo que se sabe ahora mismo.
func (r *readiness) snapshot() api.GuestReady {
	probe, hooks := r.hasProbe(), len(r.listHooks()) > 0
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked(probe, hooks)
}

func (r *readiness) stateLocked(probe, hooks bool) api.GuestReady {
	st := api.GuestReady{Probe: probe, HasHooks: hooks, Hooks: r.hooks}
	if probe {
		st.StartPeriodSeconds = serviceReadySpec().ReadyStartPeriodSeconds
	}
	switch {
	case r.hooks == api.HooksRunning:
		st.Detail = "post-restore hooks are running"
	case r.hooks == api.HooksFailed:
		st.Detail = r.hookErr
	case probe && !r.ok:
		st.Detail = r.detail
	default:
		st.Ready = true
	}
	return st
}

// check es GET /ready: ejecuta la sonda si hace falta y devuelve el estado.
func (r *readiness) check(ctx context.Context) api.GuestReady {
	probe, hooks := r.hasProbe(), len(r.listHooks()) > 0
	r.mu.Lock()
	need := probe && !r.ok && r.hooks != api.HooksRunning && r.hooks != api.HooksFailed
	env := r.env
	r.mu.Unlock()
	if need {
		r.probeMu.Lock()
		// Otro /ready pudo terminarla mientras esperábamos.
		r.mu.Lock()
		need = !r.ok
		r.mu.Unlock()
		if need {
			pctx, cancel := context.WithTimeout(ctx, probeTimeout())
			run := r.probe
			if run == nil {
				run = r.run
			}
			out, err := run(pctx, r.probePath, env)
			cancel()
			r.mu.Lock()
			if err == nil {
				r.ok, r.detail = true, ""
			} else {
				r.detail = recortar(fmt.Sprintf("%s: %v: %s", r.probePath, err, strings.TrimSpace(out)))
			}
			r.mu.Unlock()
		}
		r.probeMu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked(probe, hooks)
}

// serviceReadySpec son los plazos de la sonda que declara el servicio de la
// imagen, ya acotados; sin servicio, ceros.
func serviceReadySpec() api.ServiceSpec {
	serviceState.mu.Lock()
	svc := serviceState.svc
	serviceState.mu.Unlock()
	if svc == nil {
		return api.ServiceSpec{}
	}
	clamp := func(n int) int { return min(max(n, 0), api.MaxReadyTimeoutSeconds) }
	return api.ServiceSpec{ReadyTimeoutSeconds: clamp(svc.spec.ReadyTimeoutSeconds),
		ReadyStartPeriodSeconds: clamp(svc.spec.ReadyStartPeriodSeconds)}
}

// probeTimeout es el plazo de una ejecución de la sonda.
func probeTimeout() time.Duration {
	if n := serviceReadySpec().ReadyTimeoutSeconds; n > 0 {
		return time.Duration(n) * time.Second
	}
	return readyProbeTimeout
}

// listHooks devuelve los ganchos en orden: ficheros regulares ejecutables del
// directorio, sin ocultos ni copias de editor, como run-parts.
func (r *readiness) listHooks() []string {
	ents, err := os.ReadDir(r.hooksDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || strings.HasSuffix(n, "~") || strings.HasSuffix(n, ".disabled") {
			continue
		}
		p := filepath.Join(r.hooksDir, n)
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// startHooks lanza la tanda de ganchos en segundo plano. Devuelve false si ya
// había una corriendo (no se solapan: la segunda vería a medias lo de la
// primera). Sin ganchos no cambia nada y devuelve true. done, si no es nil, se
// llama al acabar (tests).
func (r *readiness) startHooks(kind string, done func()) bool {
	hooks := r.listHooks()
	r.mu.Lock()
	if r.hooks == api.HooksRunning {
		r.mu.Unlock()
		return false
	}
	if len(hooks) == 0 {
		r.mu.Unlock()
		if done != nil {
			done()
		}
		return true
	}
	r.hooks, r.hookErr = api.HooksRunning, ""
	env := append(append([]string(nil), r.env...), "KLING_RESTORE="+kind)
	r.mu.Unlock()

	go func() {
		if done != nil {
			defer done()
		}
		start := time.Now()
		estado, fallo := api.HooksDone, ""
		for _, h := range hooks {
			t0 := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
			out, err := r.run(ctx, h, env)
			cancel()
			if s := strings.TrimSpace(out); s != "" {
				log.Printf("post-restore %s: %s", filepath.Base(h), recortar(s))
			}
			if err != nil {
				estado = api.HooksFailed
				fallo = recortar(fmt.Sprintf("post-restore hook %s: %v: %s", filepath.Base(h), err, strings.TrimSpace(out)))
				log.Printf("post-restore %s failed after %s: %v", filepath.Base(h), time.Since(t0).Round(time.Millisecond), err)
				break
			}
			log.Printf("post-restore %s ok in %s", filepath.Base(h), time.Since(t0).Round(time.Millisecond))
		}
		r.mu.Lock()
		r.hooks, r.hookErr = estado, fallo
		r.mu.Unlock()
		if estado == api.HooksDone {
			log.Printf("post-restore: %d hook(s) done in %s", len(hooks), time.Since(start).Round(time.Millisecond))
		}
	}()
	return true
}

// runWithTimeout ejecuta path por el cosechador, con plazo, y devuelve la
// salida (stdout y stderr juntos, con tope). Al agotarse el plazo mata al grupo
// entero: una sonda que lanza un `sh -c` no deja nietos.
func runWithTimeout(ctx context.Context, path string, env []string) (string, error) {
	return runAs(ctx, path, env, nil)
}

// runProbeAsService ejecuta la sonda con el usuario del servicio, si la
// imagen declara uno con USER; si no, como root. Si el usuario no se
// resuelve, la sonda no corre (mejor "no listo" que root por accidente).
func runProbeAsService(ctx context.Context, path string, env []string) (string, error) {
	serviceState.mu.Lock()
	svc := serviceState.svc
	serviceState.mu.Unlock()
	if svc == nil || svc.spec.User == "" {
		return runAs(ctx, path, env, nil)
	}
	u, err := lookupUser(svc.root, svc.spec.User)
	if err != nil {
		return "", err
	}
	if u.home != "" {
		env = setEnv(env, "HOME", u.home)
	}
	return runAs(ctx, path, env, &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: u.groups})
}

func runAs(ctx context.Context, path string, env []string, cred *syscall.Credential) (string, error) {
	cmd := exec.CommandContext(ctx, path)
	cmd.Dir = "/"
	cmd.Env = env
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	cmd.Cancel = func() error { KillGroup(cmd); return nil }
	cmd.WaitDelay = 2 * time.Second
	out := newCappedBuffer(16 << 10)
	cmd.Stdout, cmd.Stderr = out, out
	exitCh, err := DefaultReaper.StartTracked(cmd)
	if err != nil {
		return "", err
	}
	err = WaitFor(cmd, exitCh)
	DefaultReaper.Forget(cmd.Process.Pid)
	if ctx.Err() != nil && err != nil {
		return out.String(), fmt.Errorf("timed out: %w", ctx.Err())
	}
	return out.String(), err
}

// restoreKind normaliza lo que llega a KLING_RESTORE: solo valores conocidos,
// para que quien llama no meta en el entorno de los ganchos lo que quiera.
func restoreKind(v, def string) string {
	switch v {
	case api.ResyncThaw, api.ResyncInstance, "manual":
		return v
	}
	return def
}

// recortar deja s en readyDetailMax bytes, por el final (lo último que dijo un
// programa suele ser el motivo).
func recortar(s string) string {
	if len(s) <= readyDetailMax {
		return s
	}
	return "…" + s[len(s)-readyDetailMax:]
}

// ReadyHandler sirve GET /ready.
func ReadyHandler() http.HandlerFunc { return readyState.readyHandler }

func (r *readiness) readyHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	st := r.check(req.Context())
	w.Header().Set("Content-Type", "application/json")
	if !st.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(st)
}

// HooksHandler sirve POST /hooks: vuelve a lanzar los ganchos tras restaurar.
func HooksHandler() http.HandlerFunc { return readyState.hooksHandler }

func (r *readiness) hooksHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	kind := restoreKind(req.URL.Query().Get("restore"), "manual")
	if !r.startHooks(kind, nil) {
		http.Error(w, "post-restore hooks are already running", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(r.snapshot())
}
