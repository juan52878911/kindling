package dorado

import (
	"context"
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
