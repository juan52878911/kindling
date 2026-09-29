package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// Etiquetas de un teléfono. La del teléfono y los puertos las pone el arranque
// en frío del dorado y las heredan sus clones (Commit las copia al snapshot).
const (
	labelPhone  = "kindling.phone"        // "1" en todo teléfono (y en su dorado)
	labelGolden = "kindling.phone.golden" // dorado del que sale
	labelPool   = "kindling.phone.pool"   // warming | spare | claimed (kling phone pool, mcp)
	labelPooled = "kindling.phone.pooled" // segundos unix de cuando quedó de repuesto
	labelOwner  = "kindling.phone.owner"  // quién lo usa ("mcp:<sesión>")

	poolWarming = "warming"
	poolSpare   = "spare"
	poolClaimed = "claimed"

	// phonedPort es la API de kling-phoned (docs/phoned.md); adbPort, adb.
	phonedPort = 8091
	adbPort    = 5555

	// storeNS: el token de cada clon, por id de máquina.
	storeNS = "phone"

	// annGolden: lo que se sabe del dorado (golden.go).
	annGolden = "phone"
)

// Lo que la extensión le pide al daemon. *api.Client lo cumple tal cual; en
// las pruebas, un daemon falso en memoria.
type daemon interface {
	Info(ctx context.Context) (*api.Info, error)
	List(ctx context.Context) ([]*api.Machine, error)
	Get(ctx context.Context, ref string) (*api.Machine, error)
	Run(ctx context.Context, r api.RunRequest) (*api.Machine, error)
	Remove(ctx context.Context, ref string) error
	Pause(ctx context.Context, ref string) (*api.Machine, error)
	Thaw(ctx context.Context, ref string) (*api.Machine, error)
	Freeze(ctx context.Context, ref string) (*api.Machine, error)
	PutMMDS(ctx context.Context, ref string, data any) (*api.Machine, error)
	RunHooks(ctx context.Context, ref string, wait time.Duration) (*api.ReadyResult, error)
	Guest(ctx context.Context, ref string, r api.GuestRequest) (*api.GuestResponse, error)
	Commit(ctx context.Context, ref, name string, replace bool) (*api.Snapshot, error)
	Snapshot(ctx context.Context, name string) (*api.Snapshot, error)
	SetAnnotation(ctx context.Context, name, key string, v any) (*api.Snapshot, error)
	RemoveSnapshot(ctx context.Context, name string) error
	GetStore(ctx context.Context, ns, key string, out any) error
	PutStore(ctx context.Context, ns, key string, v any) error
	DeleteStore(ctx context.Context, ns, key string) error
	StoreKeys(ctx context.Context, ns string) ([]string, error)
	SetLabels(ctx context.Context, ref string, labels map[string]string) error
	ImageRecipe(ctx context.Context, name string) (*api.ImageRecipe, error)
	Logs(ctx context.Context, ref string, tail int) (string, error)
}

// settings es la configuración de la extensión (kling config set phone.<k>).
type settings struct {
	Image      string
	Golden     string
	Prefix     string
	CPUs       int
	MemMiB     int
	Egress     string
	AdbPubKey  string
	AdbKeysDir string
	MinMem     int
}

func defaultSettings() settings {
	home, _ := os.UserHomeDir()
	return settings{Image: "android13", Golden: "phone-golden", Prefix: "phone", CPUs: 2, MemMiB: 1536,
		Egress: "none", AdbPubKey: filepath.Join(home, ".android", "adbkey.pub"), MinMem: 35}
}

func loadSettings(cfg *config.Config) settings {
	s := defaultSettings()
	str := func(k string, dst *string) {
		if v := cfg.ExtensionValue("phone", k, "string"); v != "" {
			*dst = v
		}
	}
	num := func(k string, dst *int) {
		if v := cfg.ExtensionValue("phone", k, "int"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*dst = n
			}
		}
	}
	str("image", &s.Image)
	str("golden", &s.Golden)
	str("prefix", &s.Prefix)
	str("egress", &s.Egress)
	str("adb_pubkey", &s.AdbPubKey)
	str("adb_keys_dir", &s.AdbKeysDir)
	num("cpus", &s.CPUs)
	num("mem", &s.MemMiB)
	num("min_memlevel", &s.MinMem)
	return s
}

// app es lo que usan los comandos: el daemon, la configuración y la salida.
type app struct {
	d     daemon
	s     settings
	host  string // endpoint del daemon (para saber si es local)
	out   io.Writer
	errw  io.Writer
	now   func() time.Time
	sleep func(time.Duration)
	// memLevel da el nivel de memoria libre del host del daemon (0-100) o -1
	// si no se sabe (daemon remoto).
	memLevel func() int

	namesMu  *sync.Mutex
	reserved map[string]bool
	// tokenDir sustituye al directorio local de tokens (pruebas).
	tokenDir string
}

func newApp(host string) *app {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (falling back to defaults)\n", err)
		cfg = &config.Config{}
	}
	h := cfg.Host(host)
	a := &app{d: api.NewClient(h), s: loadSettings(cfg), host: h, out: os.Stdout, errw: os.Stderr,
		now: time.Now, sleep: time.Sleep, namesMu: &sync.Mutex{}}
	a.memLevel = func() int { return localMemLevel(h) }
	return a
}

func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("H", "", "daemon endpoint (socket or ssh://user@host)")
}

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// parseFlags acepta los flags antes o después de los argumentos posicionales
// (`kling phone rm 1 -a` y `kling phone rm -a 1`).
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr(err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func usageErr(err error) error { return &plugin.ExitError{Code: 2, Err: err} }

// ── teléfonos ────────────────────────────────────────────────────────────────

var reName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// phoneName: "3" es <prefijo>-3; cualquier otra cosa, el nombre tal cual.
func (a *app) phoneName(arg string) string {
	if _, err := strconv.Atoi(arg); err == nil {
		return a.s.Prefix + "-" + arg
	}
	return arg
}

func isPhone(m *api.Machine) bool { return m != nil && m.Labels[labelPhone] == "1" }

// phones lista los teléfonos, por nombre en orden natural.
func (a *app) phones(ctx context.Context) ([]*api.Machine, error) {
	ms, err := a.d.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*api.Machine
	for _, m := range ms {
		if isPhone(m) {
			out = append(out, m)
		}
	}
	sortNatural(out)
	return out, nil
}

var reDigits = regexp.MustCompile(`\d+|\D+`)

// sortNatural: phone-2 antes que phone-10.
func sortNatural(ms []*api.Machine) {
	sort.SliceStable(ms, func(i, j int) bool { return naturalLess(ms[i].Name, ms[j].Name) })
}

func naturalLess(a, b string) bool {
	x, y := reDigits.FindAllString(a, -1), reDigits.FindAllString(b, -1)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		n, e1 := strconv.Atoi(x[i])
		m, e2 := strconv.Atoi(y[i])
		if e1 == nil && e2 == nil {
			return n < m
		}
		return x[i] < y[i]
	}
	return len(x) < len(y)
}

// phone resuelve un argumento a un teléfono (tiene que llevar su etiqueta).
func (a *app) phone(ctx context.Context, arg string) (*api.Machine, error) {
	name := a.phoneName(arg)
	m, err := a.d.Get(ctx, name)
	if err != nil {
		if api.IsNotFound(err) {
			return nil, fmt.Errorf("no phone %s (kling phone ls)", name)
		}
		return nil, err
	}
	if !isPhone(m) {
		return nil, fmt.Errorf("%s is not a phone (no %s label)", m.Name, labelPhone)
	}
	return m, nil
}

// nextName reserva el primer <prefijo>-N libre entre TODAS las máquinas. La
// reserva es del proceso (el servidor MCP y su fondo crean a la vez): dos
// teléfonos nuevos nunca eligen el mismo nombre.
func (a *app) nextName(ctx context.Context) (string, error) {
	ms, err := a.d.List(ctx)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, m := range ms {
		used[m.Name] = true
	}
	a.namesMu.Lock()
	defer a.namesMu.Unlock()
	if a.reserved == nil {
		a.reserved = map[string]bool{}
	}
	for i := 1; ; i++ {
		n := fmt.Sprintf("%s-%d", a.s.Prefix, i)
		if !used[n] && !a.reserved[n] {
			a.reserved[n] = true
			return n, nil
		}
	}
}

// unreserve suelta un nombre que no llegó a ser máquina.
func (a *app) unreserve(name string) {
	a.namesMu.Lock()
	delete(a.reserved, name)
	a.namesMu.Unlock()
}

// adbAddr: el reenvío del Mac (127.0.0.1:N) o la IP de la VM en Linux.
func adbAddr(m *api.Machine) string {
	if a := m.Forwards[strconv.Itoa(adbPort)]; a != "" {
		return a
	}
	if m.IP != "" && m.State == api.StateRunning {
		return m.Addr(adbPort)
	}
	return ""
}

// ── tokens de la API del teléfono (#110) ─────────────────────────────────────

// tokenRec es lo que se guarda en el store del daemon por teléfono. El store
// solo se lee con acceso al socket del daemon: la misma confianza que el
// proxy por el que viajan las llamadas.
type tokenRec struct {
	Machine string    `json:"machine"`
	Name    string    `json:"name"`
	Control string    `json:"control"`
	Read    []string  `json:"read,omitempty"`
	Created time.Time `json:"created"`
}

// apiToken es lo que viaja por MMDS: el sha256 del token, nunca el token.
type apiToken struct {
	SHA256 string `json:"sha256"`
	Scope  string `json:"scope"`
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "kph_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func tokenHash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// hashes son los tokens que abre la API de ese teléfono.
func (r *tokenRec) hashes() []apiToken {
	out := []apiToken{{SHA256: tokenHash(r.Control), Scope: "control"}}
	for _, t := range r.Read {
		out = append(out, apiToken{SHA256: tokenHash(t), Scope: "read"})
	}
	return out
}

func (a *app) token(ctx context.Context, m *api.Machine) (*tokenRec, error) {
	r, err := a.getToken(ctx, m.ID)
	if err != nil {
		if api.IsNotFound(err) {
			return nil, fmt.Errorf("%s has no API token in the daemon store (not made by kling phone, or a copy of one): "+
				"remove it (kling phone rm %s) or make a new one", m.Name, m.Name)
		}
		return nil, err
	}
	if r.Machine != m.ID || r.Control == "" {
		return nil, fmt.Errorf("%s: the stored API token belongs to another machine", m.Name)
	}
	return r, nil
}

// ── la API del teléfono, por el proxy del daemon ─────────────────────────────

// phoneResp es la respuesta de kling-phoned, con el cuerpo ya decodificado.
type phoneResp struct {
	Status      int
	Body        []byte
	ContentType string
	Source      string
}

// apiError es una respuesta no 2xx de kling-phoned.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("phone API: HTTP %d: %s", e.Status, e.Msg) }

// call manda una petición a kling-phoned. El cuerpo binario (un APK) va en
// base64 (?encoding=base64): el proxy lleva el cuerpo como una cadena JSON,
// que no es binario limpio. Las respuestas binarias (screen) se piden en
// base64 y se decodifican aquí. tok vacío: sin token (solo /v1/health).
func (a *app) call(ctx context.Context, m *api.Machine, tok, method, path string, body []byte, binaryBody bool) (*phoneResp, error) {
	u, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(u.Path, "/") {
		return nil, fmt.Errorf("bad path %q", path)
	}
	q := u.Query()
	wantBinary := method == "GET" && (u.Path == "/v1/screen")
	if binaryBody || wantBinary {
		q.Set("encoding", "base64")
		u.RawQuery = q.Encode()
	}
	sb := string(body)
	if binaryBody {
		sb = base64.StdEncoding.EncodeToString(body)
	}
	hdr := map[string]string{"Content-Type": "application/json"}
	if tok != "" {
		hdr["Authorization"] = "Bearer " + tok
	}
	r, err := a.d.Guest(ctx, m.ID, api.GuestRequest{
		Port: phonedPort, Path: u.RequestURI(), Method: method, Body: sb, Headers: hdr,
		ResponseHeaders: []string{"Content-Type", "X-Phoned-Source", "X-Phoned-Content-Type"},
		MaxBodyBytes:    48 << 20, WaitMS: 5000,
	})
	if err != nil {
		return nil, err
	}
	out := &phoneResp{Status: r.Status, Body: []byte(r.Body), ContentType: r.Headers["Content-Type"], Source: r.Headers["X-Phoned-Source"]}
	if ct := r.Headers["X-Phoned-Content-Type"]; ct != "" && r.Status/100 == 2 {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Body))
		if err != nil {
			return nil, fmt.Errorf("phone API: bad base64 body: %v", err)
		}
		out.Body, out.ContentType = dec, ct
	}
	if out.Status/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(out.Body))
		if json.Unmarshal(out.Body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return out, &apiError{Status: out.Status, Msg: msg}
	}
	return out, nil
}

// callPhone es call con el token de control del teléfono.
func (a *app) callPhone(ctx context.Context, m *api.Machine, method, path string, body []byte, binaryBody bool) (*phoneResp, error) {
	rec, err := a.token(ctx, m)
	if err != nil {
		return nil, err
	}
	return a.call(ctx, m, rec.Control, method, path, body, binaryBody)
}

// health es GET /v1/health (abierto: no necesita token).
type health struct {
	OK            bool   `json:"ok"`
	State         string `json:"state"`
	BootCompleted bool   `json:"boot_completed"`
	SystemServer  bool   `json:"system_server"`
	Restarts      int    `json:"restarts"`
	Net           string `json:"net"`
	Verity        string `json:"verity"`
	Uidump        bool   `json:"uidump"`
	AdbSecure     bool   `json:"adb_secure"`
	Version       string `json:"version"`
	Kernel        string `json:"kernel"`
	APITokens     int    `json:"api_tokens"`
	Detail        string `json:"detail"`
}

func (a *app) health(ctx context.Context, m *api.Machine) (*health, error) {
	r, err := a.call(ctx, m, "", "GET", "/v1/health", nil, false)
	if r == nil {
		return nil, err
	}
	var h health
	if jerr := json.Unmarshal(r.Body, &h); jerr != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("phone API: /v1/health is not JSON: %v", jerr)
	}
	return &h, nil
}

// waitHealthy espera a que /v1/health diga ok.
func (a *app) waitHealthy(ctx context.Context, m *api.Machine, timeout time.Duration) (*health, error) {
	deadline := a.now().Add(timeout)
	var last error
	for {
		h, err := a.health(ctx, m)
		if err == nil && h.OK {
			return h, nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("not healthy: state %q %s", h.State, h.Detail)
		}
		if a.now().After(deadline) || ctx.Err() != nil {
			return h, fmt.Errorf("%s: %w", m.Name, last)
		}
		a.sleep(300 * time.Millisecond)
	}
}

// ── identidad por clon (#92) ─────────────────────────────────────────────────

type phoneDoc struct {
	AndroidID string      `json:"android_id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Serial    string      `json:"serial,omitempty"`
	AdbKeys   []string    `json:"adb_keys,omitempty"`
	APITokens *[]apiToken `json:"api_tokens,omitempty"`
}

type identityDoc struct {
	Phone phoneDoc `json:"phone"`
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// adbKeys son las claves públicas de adb que abren ESTE teléfono: la suya
// (adb_keys_dir/<nombre>.pub) o la del usuario (adb_pubkey).
func (a *app) adbKeys(name string) []string {
	var files []string
	if a.s.AdbKeysDir != "" {
		files = append(files, filepath.Join(a.s.AdbKeysDir, name+".pub"))
	}
	files = append(files, a.s.AdbPubKey)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var keys []string
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				keys = append(keys, l)
			}
		}
		if len(keys) > 0 {
			return keys
		}
	}
	return nil
}

// newIdentity: un android_id y una serie nuevos, el nombre, las claves de adb
// y el token de la API (su sha256).
func (a *app) newIdentity(name string, rec *tokenRec) (identityDoc, error) {
	id, err := randHex(8)
	if err != nil {
		return identityDoc{}, err
	}
	serial, err := randHex(8)
	if err != nil {
		return identityDoc{}, err
	}
	toks := rec.hashes()
	return identityDoc{Phone: phoneDoc{AndroidID: id, Name: name, Serial: strings.ToUpper(serial),
		AdbKeys: a.adbKeys(name), APITokens: &toks}}, nil
}

// applyDoc entrega un documento por MMDS, lanza los ganchos de la imagen
// (10-identity lo aplica) y vacía MMDS: el daemon levanta la marca de
// secretos y el teléfono se puede pausar, congelar y guardar.
func (a *app) applyDoc(ctx context.Context, m *api.Machine, doc any) error {
	if _, err := a.d.PutMMDS(ctx, m.ID, doc); err != nil {
		return fmt.Errorf("%s: MMDS: %w", m.Name, err)
	}
	res, err := a.d.RunHooks(ctx, m.ID, 90*time.Second)
	// Vaciar siempre, también si el gancho falló: la identidad no se queda en MMDS.
	if _, cerr := a.d.PutMMDS(context.WithoutCancel(ctx), m.ID, map[string]any{}); cerr != nil && err == nil {
		err = fmt.Errorf("emptying MMDS: %w", cerr)
	}
	if err != nil {
		return fmt.Errorf("%s: post-restore hooks: %w", m.Name, err)
	}
	if !res.OK() {
		detail := ""
		if res.Guest != nil {
			detail = ": " + res.Guest.Detail
		}
		return fmt.Errorf("%s: post-restore hooks did not finish (%s)%s", m.Name, res.Ready, detail)
	}
	return nil
}

// ── memoria del host ─────────────────────────────────────────────────────────

// localMemLevel: 0-100 de memoria libre si el daemon es local (socket unix),
// -1 si no se sabe. macOS: kern.memorystatus_level; Linux: MemAvailable.
func localMemLevel(host string) int {
	if strings.HasPrefix(host, "ssh://") || strings.HasPrefix(host, "tcp://") {
		return -1
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "kern.memorystatus_level").Output()
		if err != nil {
			return -1
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return -1
		}
		return n
	case "linux":
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return -1
		}
		var total, avail int
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			n, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "MemTotal:":
				total = n
			case "MemAvailable:":
				avail = n
			}
		}
		if total == 0 {
			return -1
		}
		return avail * 100 / total
	}
	return -1
}

// waitMemory no arranca otro teléfono con el host justo de memoria: bajo
// presión, el Mac ha repartido páginas a ceros a un dorado (docs/telefono.md).
func (a *app) waitMemory(ctx context.Context) error {
	if a.memLevel == nil {
		return nil
	}
	deadline := a.now().Add(5 * time.Minute)
	warned := false
	for {
		lvl := a.memLevel()
		if lvl < 0 || lvl >= a.s.MinMem {
			return nil
		}
		if a.now().After(deadline) {
			return fmt.Errorf("host memory level is %d (< %d) for 5 min; free memory or pause/rm phones", lvl, a.s.MinMem)
		}
		if !warned {
			fmt.Fprintf(a.errw, "==> host memory low (level %d < %d); waiting\n", lvl, a.s.MinMem)
			warned = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		a.sleep(3 * time.Second)
	}
}

// secs formatea una duración como en phone.sh.
func secs(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + " s" }

var errNoGolden = errors.New("no golden")
