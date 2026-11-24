package dorado

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// clienteFalso es un daemon de mentira en memoria: apunta cada llamada y
// contesta lo que se le diga, sin microVMs.
type clienteFalso struct {
	mu        sync.Mutex
	llamadas  []string
	run       *api.RunRequest
	commit    string
	replace   bool
	borradas  []string
	cargando  int // respuestas 503 de la sonda antes de la buena
	salud     int // código de la sonda cuando ya no carga (0 = 200)
	warmCod   int // código de cada petición de calentamiento (0 = 200)
	cuerpos   []string
	cabeceras []map[string]string
	puertos   []int
	// eco hace que cada petición de calentamiento devuelva en el cuerpo las
	// cabeceras que recibió (un backend que repite lo que le llega).
	eco bool
}

func (f *clienteFalso) apunta(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.llamadas = append(f.llamadas, s)
}

func (f *clienteFalso) Run(_ context.Context, r api.RunRequest) (*api.Machine, error) {
	f.apunta("run")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run = &r
	return &api.Machine{ID: "m1", Name: r.Name, BootMS: 7, Labels: r.Labels}, nil
}

func (f *clienteFalso) Guest(_ context.Context, _ string, g api.GuestRequest) (*api.GuestResponse, error) {
	f.mu.Lock()
	f.puertos = append(f.puertos, g.Port)
	f.mu.Unlock()
	if g.ProbeOnly {
		f.apunta("probe")
		return &api.GuestResponse{Status: 200}, nil
	}
	f.apunta(g.Method + " " + g.Path)
	f.mu.Lock()
	defer f.mu.Unlock()
	if g.Path == "/healthz" {
		if f.cargando > 0 {
			f.cargando--
			return &api.GuestResponse{Status: 503, Body: "loading"}, nil
		}
		return &api.GuestResponse{Status: cmp(f.salud, 200), Body: "ok"}, nil
	}
	f.cuerpos = append(f.cuerpos, g.Body)
	f.cabeceras = append(f.cabeceras, g.Headers)
	if f.eco {
		return &api.GuestResponse{Status: cmp(f.warmCod, 200), Body: fmt.Sprintf("recibido: %v", g.Headers)}, nil
	}
	return &api.GuestResponse{Status: cmp(f.warmCod, 200), Body: `{"ok":true}`}, nil
}

func cmp(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func (f *clienteFalso) Squeeze(context.Context, string) (*api.SqueezeResult, error) {
	f.apunta("squeeze")
	return &api.SqueezeResult{ReclaimedMiB: 3}, nil
}

func (f *clienteFalso) Commit(_ context.Context, _ string, name string, replace bool) (*api.Snapshot, error) {
	f.apunta("commit")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commit, f.replace = name, replace
	return &api.Snapshot{Name: name, MemBytes: 1 << 20}, nil
}

func (f *clienteFalso) Remove(_ context.Context, ref string) error {
	f.apunta("rm")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.borradas = append(f.borradas, ref)
	return nil
}

func opcionesBase() Options {
	return Options{
		Image: "srv", Snapshot: "srv-v1", Port: 8090, VCPUs: 2, MemMiB: 256,
		Health: Probe{Path: "/healthz"},
		Warmup: []Request{
			{Method: "POST", Path: "/v1/decide", Body: []byte(`{"q":"sintético"}`)},
			{Method: "GET", Path: "/v1/info"},
		},
		Wait: 2 * time.Second,
	}
}

func TestMakeFlujo(t *testing.T) {
	f := &clienteFalso{cargando: 2}
	r, err := Make(context.Background(), f, opcionesBase())
	if err != nil {
		t.Fatal(err)
	}
	if r.Snapshot.Name != "srv-v1" || r.BootMS != 7 || r.Warmups != 2 {
		t.Fatalf("resultado: %+v", r)
	}
	quiero := "run probe GET /healthz GET /healthz GET /healthz POST /v1/decide GET /v1/info squeeze commit rm"
	if got := strings.Join(f.llamadas, " "); got != quiero {
		t.Fatalf("llamadas:\n got %s\nwant %s", got, quiero)
	}
	for _, p := range f.puertos {
		if p != 8090 {
			t.Fatalf("una petición fue al puerto %d, no al declarado", p)
		}
	}
	// La plantilla arranca sin red y con el puerto declarado para el proxy.
	if f.run.Egress != "none" || f.run.Labels[api.LabelPorts] != "8090" || f.run.Labels[api.LabelService] != "srv-v1" ||
		!strings.HasPrefix(f.run.Name, "srv-v1-golden-") {
		t.Fatalf("run: %+v", f.run)
	}
	if f.cuerpos[0] != `{"q":"sintético"}` || f.cuerpos[1] != "" {
		t.Fatalf("cuerpos: %q", f.cuerpos)
	}
}

func TestMakeRechazaShares(t *testing.T) {
	f := &clienteFalso{}
	o := opcionesBase()
	o.Shares = []api.ShareSpec{{Mode: "ro", Mount: "/modelos", Source: "/srv/modelos"}}
	_, err := Make(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "share") {
		t.Fatalf("una carpeta compartida debería rechazarse: %v", err)
	}
	if len(f.llamadas) != 0 {
		t.Fatalf("no debería tocar el daemon: %v", f.llamadas)
	}
}

func TestMakeRechazaEnvSecreto(t *testing.T) {
	for _, env := range []string{"OPENAI_API_KEY=x", "github_token=y", "DB_PASSWORD=z", "CLIENT_SECRET=w", "ApiKey=v"} {
		f := &clienteFalso{}
		o := opcionesBase()
		o.Env = []string{"LOG_LEVEL=debug", env}
		_, err := Make(context.Background(), f, o)
		if err == nil || !strings.Contains(err.Error(), "-allow-env") {
			t.Fatalf("%s debería rechazarse: %v", env, err)
		}
		if len(f.llamadas) != 0 {
			t.Fatalf("%s: no debería tocar el daemon: %v", env, f.llamadas)
		}
	}
	// Con AllowEnv pasa, y el entorno llega a la máquina.
	f := &clienteFalso{}
	o := opcionesBase()
	o.Env, o.AllowEnv = []string{"API_KEY=x"}, true
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if len(f.run.Env) != 1 || f.run.Env[0] != "API_KEY=x" {
		t.Fatalf("env: %v", f.run.Env)
	}
	// Uno inocente no necesita nada.
	f = &clienteFalso{}
	o = opcionesBase()
	o.Env = []string{"LOG_LEVEL=debug", "KEYBOARD_LAYOUT=es"}
	if _, err := Make(context.Background(), f, o); err == nil {
		t.Fatal("KEYBOARD_LAYOUT contiene KEY: la regla es por subcadena, como dice el plan")
	}
	o.Env = []string{"LOG_LEVEL=debug", "GOMEMLIMIT=900MiB"}
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
}

func TestMakeFallaSiWarmupNo2xx(t *testing.T) {
	f := &clienteFalso{warmCod: 422}
	_, err := Make(context.Background(), f, opcionesBase())
	if err == nil || !strings.Contains(err.Error(), "warm-up") || !strings.Contains(err.Error(), "422") {
		t.Fatalf("un calentamiento que no da 2xx debe tumbar el dorado: %v", err)
	}
	if f.commit != "" {
		t.Fatal("no debería haber hecho commit")
	}
	if len(f.borradas) != 1 {
		t.Fatalf("la plantilla debe borrarse igual: %v", f.borradas)
	}
	// 201 y 204 también son 2xx.
	f = &clienteFalso{warmCod: 204}
	if _, err := Make(context.Background(), f, opcionesBase()); err != nil {
		t.Fatal(err)
	}
}

func TestMakeFijaCPUPct(t *testing.T) {
	f := &clienteFalso{}
	if _, err := Make(context.Background(), f, opcionesBase()); err != nil {
		t.Fatal(err)
	}
	if f.run.CPUPct != 200 || f.run.VCPUs != 2 || f.run.MemMiB != 256 {
		t.Fatalf("2 vCPU deberían grabar cpu_pct 200: %+v", f.run)
	}
	f = &clienteFalso{}
	o := opcionesBase()
	o.CPUPct = 150
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if f.run.CPUPct != 150 {
		t.Fatalf("el explícito manda: %d", f.run.CPUPct)
	}
}

func TestMakeSondaExpect(t *testing.T) {
	// El servidor contesta 204 en su sonda: sin Expect nunca está listo.
	f := &clienteFalso{salud: 204}
	o := opcionesBase()
	o.Wait = 200 * time.Millisecond
	_, err := Make(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "204") {
		t.Fatalf("sin Expect 204 no debería darlo por listo: %v", err)
	}
	if f.commit != "" || len(f.borradas) != 1 {
		t.Fatalf("commit %q, borradas %v", f.commit, f.borradas)
	}
	f = &clienteFalso{salud: 204}
	o.Health.Expect = 204
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
}

func TestMakeGanchos(t *testing.T) {
	f := &clienteFalso{}
	o := opcionesBase()
	o.Warmup = nil
	var orden []string
	o.Ready = func(context.Context, string, time.Duration) error { orden = append(orden, "ready"); return nil }
	o.AfterWarmup = func(context.Context, string) error { orden = append(orden, "after"); return nil }
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	// Ready sustituye a la sonda: ni probe ni /healthz.
	if got := strings.Join(f.llamadas, " "); got != "run squeeze commit rm" || strings.Join(orden, " ") != "ready after" {
		t.Fatalf("llamadas %s, ganchos %v", got, orden)
	}
	f = &clienteFalso{}
	o.AfterWarmup = func(context.Context, string) error { return errors.New("boom") }
	if _, err := Make(context.Background(), f, o); err == nil || f.commit != "" || len(f.borradas) != 1 {
		t.Fatalf("un gancho que falla tumba el dorado: %v %q %v", err, f.commit, f.borradas)
	}
}

func TestMakeValida(t *testing.T) {
	for _, mal := range []func(*Options){
		func(o *Options) { o.Image = "" },
		func(o *Options) { o.Snapshot = "" },
		func(o *Options) { o.Port = 0 },
		func(o *Options) { o.Port = 70000 },
		func(o *Options) { o.Health.Path = "healthz" },
		func(o *Options) { o.Warmup[0].Path = "http://otro/" },
		func(o *Options) { o.Warmup[0].Method = "CONNECT" },
		func(o *Options) { o.Env = []string{"SIN_IGUAL"} },
	} {
		f := &clienteFalso{}
		o := opcionesBase()
		o.Warmup = append([]Request(nil), o.Warmup...)
		mal(&o)
		if _, err := Make(context.Background(), f, o); err == nil || len(f.llamadas) != 0 {
			t.Fatalf("opciones inválidas %+v: err %v, llamadas %v", o, err, f.llamadas)
		}
	}
}

func TestParseWarmup(t *testing.T) {
	in := `{"method":"POST","path":"/v1/decide","body":{"pregunta":"¿qué hora es?"}}

{"path":"/v1/info"}
{"method":"post","path":"/raw","body":"texto plano"}
`
	rs, err := ParseWarmup(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[0].Method != "POST" || string(rs[0].Body) != `{"pregunta":"¿qué hora es?"}` ||
		rs[1].Method != "GET" || rs[2].Method != "POST" {
		t.Fatalf("%+v", rs)
	}
	if got := rs[2].bodyText(); got != "texto plano" {
		t.Fatalf("un cuerpo de texto JSON se manda sin comillas: %q", got)
	}
	for _, mal := range []string{
		`{"method":"POST"}`,
		`{"path":"sin-barra"}`,
		`{"path":"/x","extra":1}`,
		`nope`,
	} {
		if _, err := ParseWarmup(strings.NewReader(mal)); err == nil {
			t.Fatalf("%s debería rechazarse", mal)
		}
	}
	var muchas strings.Builder
	for i := 0; i <= maxWarmup; i++ {
		fmt.Fprintf(&muchas, `{"path":"/%d"}`+"\n", i)
	}
	if _, err := ParseWarmup(strings.NewReader(muchas.String())); err == nil {
		t.Fatal("más de maxWarmup peticiones debería rechazarse")
	}
}

func TestVersiones(t *testing.T) {
	if Versioned("chispa-acme", 3) != "chispa-acme-v3" {
		t.Fatal(Versioned("chispa-acme", 3))
	}
	for snap, quiero := range map[string]int{"chispa-acme-v3": 3, "chispa-acme-v12": 12} {
		if n, ok := ParseVersion("chispa-acme", snap); !ok || n != quiero {
			t.Fatalf("%s: %d %v", snap, n, ok)
		}
	}
	for _, snap := range []string{"chispa-acme", "chispa-acme-v", "chispa-acme-vx", "chispa-acme-v0", "otro-v3", "chispa-acme-v3-v4", "chispa-acme-v03"} {
		if _, ok := ParseVersion("chispa-acme", snap); ok {
			t.Fatalf("%s no es una versión de chispa-acme", snap)
		}
	}
	if n := NextVersion("m", []string{"m-v1", "m-v7", "m", "n-v9", "m-v3"}); n != 8 {
		t.Fatalf("siguiente: %d", n)
	}
	if n := NextVersion("m", nil); n != 1 {
		t.Fatalf("primera: %d", n)
	}
}

// clienteConInfo es clienteFalso con GET /info: el backend del daemon dice si
// su commit ya aprieta la memoria antes de volcar.
type clienteConInfo struct {
	*clienteFalso
	backend string
	timings *api.CommitTimings
}

func (c *clienteConInfo) Info(context.Context) (*api.Info, error) {
	c.apunta("info")
	return &api.Info{Backend: c.backend}, nil
}

func (c *clienteConInfo) Commit(ctx context.Context, ref, name string, replace bool) (*api.Snapshot, error) {
	s, err := c.clienteFalso.Commit(ctx, ref, name, replace)
	if err == nil {
		s.Timings = c.timings
	}
	return s, err
}

func contar(ls []string, s string) int {
	n := 0
	for _, l := range ls {
		if l == s {
			n++
		}
	}
	return n
}

// C9-03: con Firecracker el commit ya aprieta (apretarAntesDeVolcar) y el
// apretón de Make era el mismo trabajo dos veces seguidas. Con un backend cuyo
// commit no aprieta (macOS) o sin saberlo (daemon sin /info), Make sigue
// apretando: el dorado no puede llevar la memoria libre.
func TestMakeAprietaUnaVez(t *testing.T) {
	tim := &api.CommitTimings{SqueezeMS: 1200, DumpMS: 9000, DigestMS: 3000, TotalMS: 14000}
	var lineas []string
	c := &clienteConInfo{clienteFalso: &clienteFalso{}, backend: "firecracker", timings: tim}
	o := opcionesBase()
	o.Log = func(f string, a ...any) { lineas = append(lineas, fmt.Sprintf(f, a...)) }
	r, err := Make(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	if n := contar(c.llamadas, "squeeze"); n != 0 {
		t.Fatalf("con Firecracker el commit aprieta: Make no debe apretar otra vez (%d): %v", n, c.llamadas)
	}
	// Lo que costó el commit (y su apretón) se cuenta: C9-12.
	todo := strings.Join(lineas, "\n")
	if !strings.Contains(todo, "squeeze 1.2s") || !strings.Contains(todo, "dump 9s") || r.CommitTime <= 0 {
		t.Fatalf("el log debe llevar el desglose del commit:\n%s\n%+v", todo, r)
	}
	for _, b := range []string{"vz", "hv"} {
		c := &clienteConInfo{clienteFalso: &clienteFalso{}, backend: b}
		if _, err := Make(context.Background(), c, opcionesBase()); err != nil {
			t.Fatal(err)
		}
		if n := contar(c.llamadas, "squeeze"); n != 1 {
			t.Fatalf("%s: el commit no aprieta, Make sí, una vez: %v", b, c.llamadas)
		}
	}
	f := &clienteFalso{}
	if _, err := Make(context.Background(), f, opcionesBase()); err != nil {
		t.Fatal(err)
	}
	if n := contar(f.llamadas, "squeeze"); n != 1 {
		t.Fatalf("sin /info, lo de siempre: %v", f.llamadas)
	}
}

// P2: las cabeceras no secretas del JSONL llegan al invitado tal cual.
func TestWarmupCabeceras(t *testing.T) {
	in := `{"method":"POST","path":"/v1/decide","headers":{"X-Request-Id":"warm-1","Accept":"application/json"},"body":{}}`
	rs, err := ParseWarmup(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	f := &clienteFalso{}
	o := opcionesBase()
	o.Warmup = rs
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	h := f.cabeceras[0]
	if len(h) != 2 || h["X-Request-Id"] != "warm-1" || h["Accept"] != "application/json" {
		t.Fatalf("cabeceras: %v", h)
	}
	for _, mal := range []string{
		`{"path":"/x","headers":{"Transfer-Encoding":"chunked"}}`,
		`{"path":"/x","headers":{"Host":"otro"}}`,
		`{"path":"/x","headers":{"Mal Nombre":"v"}}`,
		`{"path":"/x","headers":{"X-A":"a\r\nX-B: b"}}`,
		`{"path":"/x","headers":{"X-A":"` + strings.Repeat("v", 4097) + `"}}`,
	} {
		if _, err := ParseWarmup(strings.NewReader(mal)); err == nil {
			t.Fatalf("%.60s debería rechazarse", mal)
		}
	}
	var muchas strings.Builder
	muchas.WriteString(`{"path":"/x","headers":{`)
	for i := 0; i < 17; i++ {
		if i > 0 {
			muchas.WriteString(",")
		}
		fmt.Fprintf(&muchas, `"X-H%d":"v"`, i)
	}
	muchas.WriteString("}}")
	if _, err := ParseWarmup(strings.NewReader(muchas.String())); err == nil {
		t.Fatal("más de 16 cabeceras debería rechazarse")
	}
}

// P2: ninguna cabecera con pinta de credencial entra en el calentamiento: su
// valor se quedaría en la memoria del dorado. El error dice las dos salidas.
func TestWarmupRechazaCredencial(t *testing.T) {
	for _, h := range []string{"Authorization", "Cookie", "X-Api-Key", "X-My-Token", "Proxy-Authorization", "X-Auth-User"} {
		in := `{"method":"POST","path":"/v1/decide","headers":{"` + h + `":"Bearer sk-de-verdad"}}`
		_, err := ParseWarmup(strings.NewReader(in))
		if err == nil || !strings.Contains(err.Error(), "-warmup-key") || !strings.Contains(err.Error(), "without credentials") {
			t.Fatalf("%s debería rechazarse con las dos salidas: %v", h, err)
		}
		if strings.Contains(err.Error(), "sk-de-verdad") {
			t.Fatalf("%s: el error repite el valor: %v", h, err)
		}
	}
	// Por Options (sin pasar por ParseWarmup) tampoco.
	f := &clienteFalso{}
	o := opcionesBase()
	o.Warmup = []Request{{Method: "GET", Path: "/x", Headers: map[string]string{"authorization": "Bearer x"}}}
	if _, err := Make(context.Background(), f, o); err == nil || len(f.llamadas) != 0 {
		t.Fatalf("err %v, llamadas %v", err, f.llamadas)
	}
	// El marcador sin -warmup-key tampoco: no habría clave que poner.
	o.Warmup[0].Headers = map[string]string{"Authorization": "Bearer " + WarmupKeyPlaceholder}
	if _, err := Make(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "-warmup-key") || len(f.llamadas) != 0 {
		t.Fatalf("err %v, llamadas %v", err, f.llamadas)
	}
}

// P2: con WarmupKey, {{warmup_key}} se cambia por una clave desechable; el
// invitado solo recibe su verificador y su caducidad.
func TestWarmupKeySustituye(t *testing.T) {
	f := &clienteFalso{}
	o := opcionesBase()
	o.WarmupKey = true
	o.Warmup = []Request{
		{Method: "POST", Path: "/v1/decide", Headers: map[string]string{"Authorization": "Bearer " + WarmupKeyPlaceholder}},
		{Method: "GET", Path: "/v1/info", Headers: map[string]string{"X-Api-Key": WarmupKeyPlaceholder}},
	}
	antes := time.Now()
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	k, ok := strings.CutPrefix(f.cabeceras[0]["Authorization"], "Bearer ")
	if !ok || len(k) < 32 || strings.Contains(k, "{{") || f.cabeceras[1]["X-Api-Key"] != k {
		t.Fatalf("cabeceras: %v", f.cabeceras)
	}
	env := map[string]string{}
	for _, kv := range f.run.Env {
		n, v, _ := strings.Cut(kv, "=")
		env[n] = v
	}
	sum := sha256.Sum256([]byte(k))
	if env[EnvWarmupKeySHA256] != hex.EncodeToString(sum[:]) {
		t.Fatalf("verificador %q, quería el sha256 de la clave", env[EnvWarmupKeySHA256])
	}
	cad, err := time.Parse(time.RFC3339, env[EnvWarmupKeyExpires])
	if err != nil || !cad.After(antes.Add(o.Wait)) || cad.After(antes.Add(o.Wait+10*time.Minute)) {
		t.Fatalf("caducidad %q (%v): ahora + Wait + margen", env[EnvWarmupKeyExpires], err)
	}
	for _, kv := range f.run.Env {
		if strings.Contains(kv, k) {
			t.Fatalf("la clave entró en el entorno: %s", kv)
		}
	}
	// Dos dorados, dos claves.
	g := &clienteFalso{}
	if _, err := Make(context.Background(), g, o); err != nil {
		t.Fatal(err)
	}
	if g.cabeceras[0]["Authorization"] == f.cabeceras[0]["Authorization"] {
		t.Fatal("la clave de calentamiento se repite entre dorados")
	}
	// Los nombres reservados no los pone quien llama.
	o.Env = []string{EnvWarmupKeySHA256 + "=otro"}
	if _, err := Make(context.Background(), &clienteFalso{}, o); err == nil {
		t.Fatal("un -e con el verificador debería rechazarse con WarmupKey")
	}
}

// P2: la clave desechable no aparece en el log, ni en el error de un 401 que
// la repite, ni en el resultado.
func TestWarmupKeyNoSeFiltra(t *testing.T) {
	f := &clienteFalso{warmCod: 401, eco: true}
	o := opcionesBase()
	o.WarmupKey = true
	o.Warmup = []Request{{Method: "POST", Path: "/v1/decide", Headers: map[string]string{"Authorization": "Bearer " + WarmupKeyPlaceholder}}}
	var log strings.Builder
	o.Log = func(format string, a ...any) { fmt.Fprintf(&log, format+"\n", a...) }
	r, err := Make(context.Background(), f, o)
	if err == nil {
		t.Fatal("un 401 debe tumbar el dorado")
	}
	k := strings.TrimPrefix(f.cabeceras[0]["Authorization"], "Bearer ")
	if len(k) < 32 {
		t.Fatalf("no se mandó la clave: %v", f.cabeceras)
	}
	for donde, txt := range map[string]string{"error": err.Error(), "log": log.String(), "resultado": fmt.Sprintf("%+v", r)} {
		if strings.Contains(txt, k) {
			t.Fatalf("la clave aparece en el %s: %s", donde, txt)
		}
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("el error debe decir el código: %v", err)
	}
	// Bien contestado, tampoco en el resultado ni en el log.
	f = &clienteFalso{eco: true}
	log.Reset()
	r, err = Make(context.Background(), f, o)
	if err != nil {
		t.Fatal(err)
	}
	k = strings.TrimPrefix(f.cabeceras[0]["Authorization"], "Bearer ")
	if strings.Contains(fmt.Sprintf("%+v %+v", r, r.Snapshot), k) || strings.Contains(log.String(), k) {
		t.Fatal("la clave aparece en el resultado o el log")
	}
}

// P2: los dos nombres internos del calentamiento no tienen pinta de secreto
// (son un verificador y una fecha); cualquier otro con KEY sigue rechazándose.
func TestEnvSecretoEximeWarmup(t *testing.T) {
	if n := EnvSecreto([]string{EnvWarmupKeySHA256 + "=ab", EnvWarmupKeyExpires + "=2026-01-01T00:00:00Z"}); n != "" {
		t.Fatalf("%s no debería contar como secreto", n)
	}
	for _, kv := range []string{"KLING_WARMUP_KEY_OTRA=x", "KLING_WARMUP_KEY=x", "KLING_WARMUP_KEY_SHA256X=x"} {
		if EnvSecreto([]string{kv}) == "" {
			t.Fatalf("%s debería contar como secreto", kv)
		}
	}
}

// clienteCredenciales es clienteFalso con lo que hace falta para dar
// credenciales del proxy a la plantilla y al dorado.
type clienteCredenciales struct {
	*clienteFalso
	maquina []api.CredentialSpec
	dorado  []api.CredentialSpec
	start   []string
}

func (c *clienteCredenciales) SetCredentials(_ context.Context, _ string, r api.CredentialsRequest) (*api.Machine, error) {
	c.apunta("credentials")
	c.maquina = r.Credentials
	return &api.Machine{ID: "m1"}, nil
}

func (c *clienteCredenciales) Stop(context.Context, string) (*api.Machine, error) {
	c.apunta("stop")
	return &api.Machine{ID: "m1"}, nil
}

func (c *clienteCredenciales) Start(_ context.Context, _ string, env []string) (*api.Machine, error) {
	c.apunta("start")
	c.start = env
	return &api.Machine{ID: "m1"}, nil
}

func (c *clienteCredenciales) SetSnapshotCredentials(_ context.Context, name string, r api.CredentialsRequest) (*api.Snapshot, error) {
	c.apunta("snapshot-credentials")
	c.dorado = r.Credentials
	return &api.Snapshot{Name: name}, nil
}

// P5: la plantilla puede salir con egress allowlist y credenciales del proxy
// (la clave nunca entra en el invitado). El servicio lee su entorno al
// arrancar, así que la plantilla se rearranca para que vea los marcadores, y
// el dorado se queda con las credenciales para sus réplicas.
func TestMakeEgressYCredenciales(t *testing.T) {
	cred := api.CredentialSpec{Domain: "l3.kindling.internal", Env: "L3_TOKEN", Secret: "s3cr3t", Allow: []string{"POST /v1/chat/completions"}}
	c := &clienteCredenciales{clienteFalso: &clienteFalso{}}
	o := opcionesBase()
	o.Egress, o.AllowDomains = "allowlist", []string{"l3.kindling.internal"}
	o.Credentials = []api.CredentialSpec{cred}
	o.Env = []string{"LOG_LEVEL=debug"}
	if _, err := Make(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	if c.run.Egress != "allowlist" || len(c.run.AllowDomains) != 1 {
		t.Fatalf("run: %+v", c.run)
	}
	got := strings.Join(c.llamadas, " ")
	if !strings.HasPrefix(got, "run credentials stop start probe") || !strings.Contains(got, "commit snapshot-credentials") {
		t.Fatalf("llamadas: %s", got)
	}
	if len(c.maquina) != 1 || c.maquina[0].Secret != "s3cr3t" || len(c.dorado) != 1 || len(c.start) != 1 {
		t.Fatalf("credenciales %v / %v, start %v", c.maquina, c.dorado, c.start)
	}
	for _, mal := range []func(*Options){
		func(o *Options) { o.Egress = "internet" },
		func(o *Options) { o.Egress = "none" },
		func(o *Options) { o.Credentials[0].Secret = "" },
		func(o *Options) { o.Credentials[0].Env = "" },
	} {
		c := &clienteCredenciales{clienteFalso: &clienteFalso{}}
		o := opcionesBase()
		o.Egress, o.Credentials = "allowlist", []api.CredentialSpec{cred}
		mal(&o)
		if _, err := Make(context.Background(), c, o); err == nil || len(c.llamadas) != 0 {
			t.Fatalf("%+v: err %v, llamadas %v", o, err, c.llamadas)
		}
	}
	// Sin el API de credenciales, se dice antes de arrancar nada.
	f := &clienteFalso{}
	o = opcionesBase()
	o.Egress, o.Credentials = "allowlist", []api.CredentialSpec{cred}
	if _, err := Make(context.Background(), f, o); err == nil || len(f.llamadas) != 0 {
		t.Fatalf("err %v, llamadas %v", err, f.llamadas)
	}
	// AllowDomains solo, sin Egress: es allowlist.
	f = &clienteFalso{}
	o = opcionesBase()
	o.AllowDomains = []string{"huggingface.co"}
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if f.run.Egress != "allowlist" {
		t.Fatalf("egress %q", f.run.Egress)
	}
}
