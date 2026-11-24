// Package dorado fabrica el dorado de un backend de modelo (plan de la ronda 8,
// §1.3): arranca la imagen, espera a que el servidor conteste su sonda, lo
// calienta con una lista declarada de peticiones, devuelve al host la memoria
// libre y lo congela como snapshot. Es el algoritmo que von.MakeGolden y `kling
// ai chispa deploy` tenían cada uno con sus rutas cableadas, y el que usa `kling
// ai model deploy` para cualquier servidor HTTP.
//
// Reglas que impone, no recomienda:
//
//   - Sin carpetas compartidas: una máquina con shares no se puede guardar como
//     dorado (internal/machine/snapshot.go), y los artefactos del modelo tienen
//     que ir dentro de la imagen para que todas las réplicas mapeen las mismas
//     páginas.
//   - Sin secretos: el entorno de la plantilla queda en la memoria del dorado y
//     en la de cada réplica. Un nombre de variable con pinta de credencial se
//     rechaza salvo AllowEnv; la credencial la añade el gateway (auth).
//   - cpu_pct = 100 por vCPU salvo que se diga otro: el dorado lo graba y las
//     réplicas lo heredan, y el 50 % por defecto del daemon (pensado para
//     herramientas MCP) deja a un modelo a una fracción de su velocidad.
//
// No importa internal/aigw a propósito: aigw importa von, y von usa este
// paquete. Por eso la sonda es su propio tipo (Probe) y no aigw.Health.
package dorado

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Etiquetas que `kling ai model deploy` pone en sus dorados: de qué modelo del
// registro es y qué versión. `kling ai model roll` las lee para elegir.
const (
	LabelModel   = "dorado.model"
	LabelVersion = "dorado.version"
)

// Client es lo que el fabricante usa del daemon; *api.Client lo cumple. Una
// interfaz para que los tests lo prueben con un daemon de mentira.
type Client interface {
	Run(ctx context.Context, r api.RunRequest) (*api.Machine, error)
	Guest(ctx context.Context, ref string, r api.GuestRequest) (*api.GuestResponse, error)
	Squeeze(ctx context.Context, ref string) (*api.SqueezeResult, error)
	Commit(ctx context.Context, ref, name string, replace bool) (*api.Snapshot, error)
	Remove(ctx context.Context, ref string) error
}

// Probe es la sonda de salud del servidor: GET Path hasta que conteste Expect.
// Con Path vacío basta con que el puerto acepte conexiones.
type Probe struct {
	Path string
	// Expect es el código que da por bueno el servidor; 0 = 200.
	Expect int
	// Timeout acota cada intento; 0 = DefaultProbeTimeout.
	Timeout time.Duration
}

// Request es una petición del calentamiento: una línea del fichero JSONL de
// `-warmup`. Body es JSON tal cual; si es una cadena JSON, se manda su texto
// (para cuerpos que no son JSON).
type Request struct {
	Method string          `json:"method,omitempty"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// bodyText es el cuerpo que se manda: el texto de una cadena JSON, o el JSON
// tal cual.
func (r Request) bodyText() string {
	b := bytes.TrimSpace(r.Body)
	if len(b) == 0 || string(b) == "null" {
		return ""
	}
	if b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return s
		}
	}
	return string(b)
}

// Options es cómo hacer un dorado.
type Options struct {
	Image    string // imagen de la que arranca la plantilla
	Snapshot string // nombre del dorado (ya versionado si toca: Versioned)
	Port     int    // puerto del servidor en el invitado
	VCPUs    int
	MemMiB   int
	// CPUPct es el techo de CPU (% de un core) que el dorado graba y sus
	// réplicas heredan; 0 = 100 por vCPU.
	CPUPct int
	Health Probe
	// Warmup son las peticiones que se mandan, en orden, tras la sonda y antes
	// de congelar. Cada una tiene que contestar 2xx o no hay dorado.
	Warmup []Request
	// Env es el entorno KEY=valor de la plantilla (y, por el snapshot, de todas
	// las réplicas). AllowEnv deja pasar nombres con pinta de credencial.
	Env      []string
	AllowEnv bool
	// Shares solo está para rechazarse: un dorado no lleva carpetas del host.
	Shares []api.ShareSpec
	// Labels extra; kling.ports y service los pone el fabricante.
	Labels    map[string]string
	AllowExec bool
	Replace   bool
	// Wait acota la espera a que el servidor esté listo y, aparte, el
	// calentamiento; 0 = DefaultWait.
	Wait time.Duration
	// Ready, si no es nil, sustituye a la sonda (puerto y Health): para un
	// servidor que distingue «cargando» de «roto», como llama-server.
	Ready func(ctx context.Context, ref string, timeout time.Duration) error
	// AfterWarmup, si no es nil, corre tras Warmup y antes de apretar: lo que
	// no cabe en una lista fija (los prefijos de VON miran la respuesta).
	AfterWarmup func(ctx context.Context, ref string) error
	// Log recibe el progreso, línea a línea. Puede ser nil.
	Log func(format string, args ...any)
}

// Result es lo que costó cada paso.
type Result struct {
	Snapshot *api.Snapshot
	Machine  string        // nombre de la plantilla (ya borrada)
	BootMS   int64         // arranque de la microVM (lo dice el daemon)
	LoadTime time.Duration // de arrancada a sonda buena
	Warmups  int           // peticiones de calentamiento contestadas
}

// Plazos por defecto.
const (
	// ServiceExitedMark es el principio de la línea que el entrypoint de las
	// imágenes de kindling escribe en /var/log/service.log cada vez que el
	// servicio muere y lo relanza (scripts/81-base-image.sh). Semilla de la
	// ronda 9 (C9-13): es contrato entre el constructor de imágenes (Z2, que
	// no puede cambiar el texto) y el fabricante (Z3, que cuenta estas líneas
	// para distinguir "cargando" de "muere en bucle").
	ServiceExitedMark = "service exited with "

	DefaultWait         = 5 * time.Minute
	DefaultProbeTimeout = 5 * time.Second
	// warmRetry es la pausa entre reintentos de una petición de calentamiento
	// que no llegó a contestarse (error de transporte, no un código).
	warmRetry = 5 * time.Second
	// maxWarmup acota el fichero de calentamiento: es una lista de prefijos y
	// casos sintéticos, no un banco de pruebas.
	maxWarmup = 1000
	// maxWarmupLine acota cada línea del JSONL.
	maxWarmupLine = 1 << 20
)

var metodos = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

// pintaDeSecreto son las subcadenas que delatan una credencial en el nombre de
// una variable (sin distinguir mayúsculas). Es por subcadena a propósito:
// mejor rechazar KEYBOARD_LAYOUT y que lo pase -allow-env que dejar pasar
// MY_APIKEY.
var pintaDeSecreto = []string{"KEY", "TOKEN", "SECRET", "PASSWORD"}

// EnvSecreto devuelve el primer nombre de env con pinta de credencial, o "".
func EnvSecreto(env []string) string {
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(name)
		for _, s := range pintaDeSecreto {
			if strings.Contains(up, s) {
				return name
			}
		}
	}
	return ""
}

func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") &&
		!strings.ContainsAny(p, " \t\r\n#")
}

// validate comprueba las opciones ANTES de tocar el daemon.
func (o *Options) validate() error {
	switch {
	case o.Image == "":
		return errors.New("golden: no image")
	case o.Snapshot == "":
		return errors.New("golden: no snapshot name")
	case o.Port < 1 || o.Port > 65535:
		return fmt.Errorf("golden: port %d out of range", o.Port)
	case o.Health.Path != "" && !validPath(o.Health.Path):
		return fmt.Errorf("golden: health path %q must be an absolute path such as /healthz", o.Health.Path)
	case o.Health.Expect != 0 && (o.Health.Expect < 100 || o.Health.Expect > 599):
		return fmt.Errorf("golden: health expect %d is not an HTTP status", o.Health.Expect)
	case len(o.Shares) > 0:
		return errors.New("golden: a golden snapshot cannot carry shared folders (-share): bake the model's files into the image, or copy them in with kling cp before the commit")
	case len(o.Warmup) > maxWarmup:
		return fmt.Errorf("golden: %d warm-up requests, at most %d", len(o.Warmup), maxWarmup)
	}
	for i, r := range o.Warmup {
		if err := r.validate(); err != nil {
			return fmt.Errorf("golden: warm-up %d: %w", i+1, err)
		}
	}
	for _, kv := range o.Env {
		if name, _, ok := strings.Cut(kv, "="); !ok || name == "" {
			return fmt.Errorf("golden: env %q is not KEY=value", kv)
		}
	}
	if n := EnvSecreto(o.Env); n != "" && !o.AllowEnv {
		return fmt.Errorf("golden: env %s looks like a credential, and the golden snapshot keeps it in the memory of every replica: let the gateway add it (auth in ai.json), or pass -allow-env if it is not one", n)
	}
	return nil
}

func (r Request) validate() error {
	if !metodos[r.Method] {
		return fmt.Errorf("method %q: use GET, POST, PUT, PATCH or DELETE", r.Method)
	}
	if !validPath(r.Path) {
		return fmt.Errorf("path %q must be an absolute path such as /v1/decide", r.Path)
	}
	return nil
}

// Make arranca una microVM de la imagen, espera a que el servidor conteste la
// sonda, lo calienta, devuelve al host la memoria libre, la congela como el
// dorado o.Snapshot y la borra. La plantilla se borra pase lo que pase.
func Make(ctx context.Context, c Client, o Options) (*Result, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Wait <= 0 {
		o.Wait = DefaultWait
	}
	if o.CPUPct == 0 {
		o.CPUPct = 100 * max(o.VCPUs, 1)
	}
	// La plantilla lleva nombre propio y aleatorio: dos despliegues a la vez
	// del mismo modelo no deben pisarse la máquina.
	name := fmt.Sprintf("%s-golden-%s", o.Snapshot, strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36))
	labels := api.MergeLabels(o.Labels, map[string]string{
		api.LabelPorts:   strconv.Itoa(o.Port),
		api.LabelService: o.Snapshot,
	})
	mc, err := c.Run(ctx, api.RunRequest{
		Name: name, Image: o.Image, VCPUs: o.VCPUs, MemMiB: o.MemMiB,
		CPUPct:    o.CPUPct,
		Egress:    "none",
		Labels:    labels,
		Env:       o.Env,
		AllowExec: o.AllowExec,
	})
	if err != nil {
		return nil, fmt.Errorf("booting %s: %w", o.Image, err)
	}
	res := &Result{Machine: mc.Name, BootMS: mc.BootMS}
	// Pase lo que pase, la plantilla no se queda: lo que vale es el dorado.
	defer func() {
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = c.Remove(rc, mc.ID)
	}()
	logf("booted %s in %d ms; waiting for the server...", mc.Name, mc.BootMS)

	t0 := time.Now()
	ready := o.Ready
	if ready == nil {
		ready = func(ctx context.Context, ref string, timeout time.Duration) error {
			return waitHealthy(ctx, c, ref, o.Port, o.Health, timeout)
		}
	}
	if err := ready(ctx, mc.ID, o.Wait); err != nil {
		return nil, fmt.Errorf("%w\nsee the server log with:  kling exec %s -- tail -50 /var/log/service.log", err, mc.Name)
	}
	res.LoadTime = time.Since(t0)
	logf("server ready in %s", res.LoadTime.Round(time.Millisecond))

	if len(o.Warmup) > 0 {
		hasta := time.Now().Add(o.Wait)
		for i, r := range o.Warmup {
			if err := warmOne(ctx, c, mc.ID, o.Port, r, hasta, logf); err != nil {
				return nil, fmt.Errorf("warm-up %d of %d (%s %s): %w", i+1, len(o.Warmup), r.Method, r.Path, err)
			}
			res.Warmups++
		}
		logf("%d warm-up request(s) answered 2xx", res.Warmups)
	}
	if o.AfterWarmup != nil {
		if err := o.AfterWarmup(ctx, mc.ID); err != nil {
			return nil, err
		}
	}

	// Devolver al host lo reclamable (memoria libre y caché de páginas limpia)
	// ANTES de congelar: el globo deja esas páginas a cero, el commit las
	// convierte en huecos del mem.file y el dorado ocupa solo lo que el modelo
	// usa de verdad. Si la máquina no tiene globo, se congela igual.
	if sq, err := c.Squeeze(ctx, mc.ID); err == nil {
		logf("returned %d MiB of free memory to the host before freezing", sq.ReclaimedMiB)
	}
	snap, err := c.Commit(ctx, mc.ID, o.Snapshot, o.Replace)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	res.Snapshot = snap
	return res, nil
}

// waitHealthy espera a que el puerto abra (lo espera el daemon, en la red que
// ve al invitado) y luego a que Health conteste lo esperado. Sondea cada 50 ms:
// lo que se mide después no debe llevar medio intervalo de propina.
func waitHealthy(ctx context.Context, c Client, ref string, port int, h Probe, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if _, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: port, WaitMS: int(timeout / time.Millisecond), ProbeOnly: true,
	}); err != nil {
		return fmt.Errorf("the server did not open port %d: %w", port, err)
	}
	if h.Path == "" {
		return nil
	}
	expect := h.Expect
	if expect == 0 {
		expect = http.StatusOK
	}
	per := h.Timeout
	if per <= 0 {
		per = DefaultProbeTimeout
	}
	var last error
	for time.Now().Before(deadline) {
		pc, cancel := context.WithTimeout(ctx, per)
		resp, err := c.Guest(pc, ref, api.GuestRequest{Port: port, Path: h.Path, Method: http.MethodGet})
		cancel()
		switch {
		case err != nil:
			last = err
		case resp.Status != expect:
			last = fmt.Errorf("%s answered %d, want %d: %s", h.Path, resp.Status, expect, recortar(resp.Body))
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("server not healthy after %s: %w", timeout, last)
}

// warmOne manda una petición del calentamiento. Un código que no sea 2xx tumba
// el dorado en el acto; un error de transporte (el proxy del daemon se cansó
// mientras el servidor sigue con la primera petición, que trae los pesos a base
// de fallos de página) se reintenta hasta el plazo.
func warmOne(ctx context.Context, c Client, ref string, port int, r Request, hasta time.Time, logf func(string, ...any)) error {
	for {
		resp, err := c.Guest(ctx, ref, api.GuestRequest{Port: port, Path: r.Path, Method: r.Method, Body: r.bodyText()})
		if err == nil {
			if resp.Status < 200 || resp.Status > 299 {
				return fmt.Errorf("answered %d: %s", resp.Status, recortar(resp.Body))
			}
			return nil
		}
		if ctx.Err() != nil || time.Now().After(hasta) {
			return err
		}
		logf("warm-up not answered yet (%v); retrying", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(warmRetry):
		}
	}
}

// ParseWarmup lee un fichero de calentamiento: una petición JSON por línea
// ({"method":"POST","path":"/v1/decide","body":{...}}); las líneas en blanco
// no cuentan y el método por defecto es GET.
func ParseWarmup(r io.Reader) ([]Request, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxWarmupLine)
	var out []Request
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var req Request
		if err := dec.Decode(&req); err != nil {
			return nil, fmt.Errorf("warm-up line %d: %w", n, err)
		}
		req.Method = strings.ToUpper(req.Method)
		if req.Method == "" {
			req.Method = http.MethodGet
		}
		if err := req.validate(); err != nil {
			return nil, fmt.Errorf("warm-up line %d: %w", n, err)
		}
		if len(out) == maxWarmup {
			return nil, fmt.Errorf("warm-up: more than %d requests", maxWarmup)
		}
		out = append(out, req)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("warm-up: %w", err)
	}
	return out, nil
}

// Versioned es el nombre del dorado n de un modelo: <modelo>-v<n>. El plan
// decía <modelo>@<n>, pero el daemon no admite '@' en un nombre de snapshot
// (internal/machine/snapshot.go, validName); -v<n> es además lo que ya usa
// `kling ai retrain` para las versiones de una tarea.
func Versioned(model string, n int) string { return model + "-v" + strconv.Itoa(n) }

// ParseVersion dice si snapshot es una versión de model y cuál.
func ParseVersion(model, snapshot string) (int, bool) {
	rest, ok := strings.CutPrefix(snapshot, model+"-v")
	if !ok || rest == "" || rest[0] == '0' {
		return 0, false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// NextVersion es la versión siguiente a la mayor de snapshots (1 si no hay).
func NextVersion(model string, snapshots []string) int {
	top := 0
	for _, s := range snapshots {
		if n, ok := ParseVersion(model, s); ok && n > top {
			top = n
		}
	}
	return top + 1
}

func recortar(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
