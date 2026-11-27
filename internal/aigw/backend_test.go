package aigw

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, src string) *Config {
	t.Helper()
	c, err := ParseConfig(strings.NewReader(src))
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	return c
}

// TestBackendAtajos: los tres kinds de hoy se expanden al bloque de un backend
// HTTP con los valores por defecto del plan (§1.1); chispa en proceso no es
// un backend.
func TestBackendAtajos(t *testing.T) {
	c := parse(t, `{"models":{
		"q":   {"kind":"von","snapshot":"von-qwen15","max_replicas":2},
		"e":   {"kind":"embed","snapshot":"enc-e5"},
		"cm":  {"kind":"chispa","backend":"microvm","snapshot":"commits"},
		"cp":  {"kind":"chispa","path":"x.chispa"},
		"qf":  {"kind":"von","snapshot":"von-smol","idle":"freeze","max_inflight":3}
	}}`)
	q := c.Backend("q")
	if q == nil {
		t.Fatal("von: no backend")
	}
	want := &Backend{
		Name: "q", Snapshot: "von-qwen15", Port: 8000, Contract: ContractOpenAI,
		Routes: []Route{{"POST", "/v1/chat/completions"}, {"POST", "/v1/completions"}},
		Health: Health{Path: "/health"}, MaxReplicas: 2, Idle: IdleRemove,
	}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("von expands to\n%+v\nwant\n%+v", q, want)
	}
	e := c.Backend("e")
	if e == nil || e.Contract != ContractOpenAIEmbeddings || e.Port != 8000 || e.Idle != IdleRemove ||
		!reflect.DeepEqual(e.Routes, []Route{{"POST", "/v1/embeddings"}}) || e.Health.Path != "/health" {
		t.Errorf("embed expands to %+v", e)
	}
	cm := c.Backend("cm")
	if cm == nil || cm.Contract != ContractChispaClassify || cm.Port != 8000 || cm.Idle != IdleFreeze ||
		len(cm.Routes) != 0 || cm.Snapshot != "commits" {
		t.Errorf("chispa microvm expands to %+v", cm)
	}
	if b := c.Backend("cp"); b != nil {
		t.Errorf("in-process chispa is not an HTTP backend: %+v", b)
	}
	if b := c.Backend("qf"); b == nil || b.Idle != IdleFreeze || b.MaxInflight != 3 {
		t.Errorf("von with idle freeze: %+v", b)
	}
	if b := c.Backend("nope"); b != nil {
		t.Errorf("unknown model: %+v", b)
	}
	var nombres []string
	for _, b := range c.Backends() {
		nombres = append(nombres, b.Name)
	}
	if !reflect.DeepEqual(nombres, []string{"cm", "e", "q", "qf"}) {
		t.Errorf("Backends() = %v", nombres)
	}
	// El resultado no depende de que Validate haya corrido: un Config hecho a
	// mano (los tests del paquete lo hacen) también se expande.
	hecho := &Config{Models: map[string]*ModelConfig{"v": {Kind: KindVON, Snapshot: "s"}}}
	if b := hecho.Backend("v"); b == nil || b.Port != 8000 || b.Contract != ContractOpenAI {
		t.Errorf("hand-built config: %+v", b)
	}
}

// TestBackendHTTPCompleto: el bloque kind http del plan, con sus campos.
func TestBackendHTTPCompleto(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vllm.key"), []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "ai.json")
	src := `{"models":{
		"chispa-acme": {"kind":"http","snapshot":"chispa-acme@3","port":8090,"contract":"chispa-decide",
			"routes":["POST /v1/decide"],"health":{"path":"/healthz","expect":200,"timeout":"5s"},
			"max_inflight":16,"max_replicas":2,"idle":"remove","tenants":["acme"]},
		"llama": {"kind":"http","snapshot":"mi-llama","port":8080,"contract":"openai",
			"health":{"path":"/health"},"max_inflight":4,"idle":"pause","limits":{"max_tokens":1024,"n":1}},
		"qwen3-gpu": {"kind":"http","upstream":"http://10.0.1.20:8000","contract":"openai",
			"upstream_model":"Qwen/Qwen3-4B","health":{"path":"/health","every":"10s"},"max_inflight":32,
			"auth":{"header":"Authorization","scheme":"Bearer","file":"vllm.key"}},
		"crudo": {"kind":"http","snapshot":"x","contract":"raw","routes":["GET /a/b","POST /c"]}
	},"tenants":[{"name":"acme","token":"0123456789abcdef"}]}`
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	a := c.Backend("chispa-acme")
	if a == nil || a.Port != 8090 || a.Contract != ContractChispaDecide ||
		!reflect.DeepEqual(a.Routes, []Route{{"POST", "/v1/decide"}}) ||
		a.Health != (Health{Path: "/healthz", Expect: 200, Timeout: 5 * time.Second}) ||
		a.MaxInflight != 16 || a.MaxReplicas != 2 || a.Idle != IdleRemove || !reflect.DeepEqual(a.Tenants, []string{"acme"}) {
		t.Errorf("chispa-acme: %+v", a)
	}
	l := c.Backend("llama")
	if l == nil || l.Idle != IdlePause || l.Limits != (Limits{MaxTokens: 1024, N: 1}) ||
		!reflect.DeepEqual(l.Routes, []Route{{"POST", "/v1/chat/completions"}, {"POST", "/v1/completions"}}) {
		t.Errorf("llama: %+v", l)
	}
	g := c.Backend("qwen3-gpu")
	if g == nil || g.Snapshot != "" || g.Upstream != "http://10.0.1.20:8000" || g.UpstreamModel != "Qwen/Qwen3-4B" ||
		g.Health.Every != 10*time.Second || g.MaxInflight != 32 || g.Auth == nil ||
		g.Auth.File != filepath.Join(dir, "vllm.key") || g.Auth.Header != "Authorization" || g.Auth.Scheme != "Bearer" {
		t.Errorf("qwen3-gpu: %+v (auth %+v)", g, g.Auth)
	}
	if r := c.Backend("crudo"); r == nil || !reflect.DeepEqual(r.Routes, []Route{{"GET", "/a/b"}, {"POST", "/c"}}) ||
		r.Health.Path != "" || r.Port != 8000 {
		t.Errorf("crudo: %+v", r)
	}
	if !c.NeedsDaemon() {
		t.Error("http backends with a snapshot need the daemon")
	}
}

// TestNeedsDaemonSoloUpstream: un registro de solo upstreams (y Chispa en
// proceso) no despierta nada en el daemon.
func TestNeedsDaemonSoloUpstream(t *testing.T) {
	c := parse(t, `{"models":{
		"g":{"kind":"http","upstream":"https://api.example.com/v1","contract":"openai"},
		"j":{"kind":"chispa","path":"x.chispa"}}}`)
	if c.NeedsDaemon() {
		t.Error("only upstreams: NeedsDaemon must be false")
	}
	if b := c.Backend("g"); b == nil || b.Upstream != "https://api.example.com/v1" {
		t.Errorf("upstream: %+v", b)
	}
}

// TestBackendHTTPValida: configuraciones malas de los campos nuevos que
// ParseConfig tiene que rechazar (con la semilla se cargaban sin validar).
func TestBackendHTTPValida(t *testing.T) {
	const ten = `,"tenants":[{"name":"acme","token":"0123456789abcdef"}]`
	malas := map[string]string{
		"snapshot y upstream":     `{"kind":"http","snapshot":"s","upstream":"http://h:1","contract":"openai"}`,
		"ni snapshot ni upstream": `{"kind":"http","contract":"openai"}`,
		"upstream ftp":            `{"kind":"http","upstream":"ftp://h/x","contract":"openai"}`,
		"upstream sin host":       `{"kind":"http","upstream":"http:///v1","contract":"openai"}`,
		"upstream con usuario":    `{"kind":"http","upstream":"https://u:p@h/v1","contract":"openai"}`,
		"upstream con query":      `{"kind":"http","upstream":"https://h/v1?key=x","contract":"openai"}`,
		"puerto 70000":            `{"kind":"http","snapshot":"s","port":70000,"contract":"openai"}`,
		"puerto negativo":         `{"kind":"http","snapshot":"s","port":-1,"contract":"openai"}`,
		"puerto con upstream":     `{"kind":"http","upstream":"http://h:1","port":80,"contract":"openai"}`,
		"sin contrato":            `{"kind":"http","snapshot":"s"}`,
		"contrato desconocido":    `{"kind":"http","snapshot":"s","contract":"grpc"}`,
		"chispa-classify en http": `{"kind":"http","snapshot":"s","contract":"chispa-classify"}`,
		"raw sin rutas":           `{"kind":"http","snapshot":"s","contract":"raw"}`,
		"ruta sin método":         `{"kind":"http","snapshot":"s","contract":"raw","routes":["/v1/decide"]}`,
		"ruta método raro":        `{"kind":"http","snapshot":"s","contract":"raw","routes":["post /v1/decide"]}`,
		"ruta con ..":             `{"kind":"http","snapshot":"s","contract":"raw","routes":["GET /v1/../admin"]}`,
		"ruta con %":              `{"kind":"http","snapshot":"s","contract":"raw","routes":["GET /v1/%2e%2e/admin"]}`,
		"ruta relativa":           `{"kind":"http","snapshot":"s","contract":"raw","routes":["GET v1/x"]}`,
		"ruta con query":          `{"kind":"http","snapshot":"s","contract":"raw","routes":["GET /v1/x?a=1"]}`,
		"ruta repetida":           `{"kind":"http","snapshot":"s","contract":"raw","routes":["GET /a","GET /a"]}`,
		"ruta con //":             `{"kind":"http","snapshot":"s","contract":"raw","routes":["GET /a//b"]}`,
		"idle desconocido":        `{"kind":"http","snapshot":"s","contract":"openai","idle":"sleep"}`,
		"idle con upstream":       `{"kind":"http","upstream":"http://h:1","contract":"openai","idle":"remove"}`,
		"max_replicas upstream":   `{"kind":"http","upstream":"http://h:1","contract":"openai","max_replicas":2}`,
		"max_inflight negativo":   `{"kind":"http","snapshot":"s","contract":"openai","max_inflight":-1}`,
		"health relativa":         `{"kind":"http","snapshot":"s","contract":"openai","health":{"path":"health"}}`,
		"health expect 42":        `{"kind":"http","snapshot":"s","contract":"openai","health":{"path":"/h","expect":42}}`,
		"health timeout malo":     `{"kind":"http","snapshot":"s","contract":"openai","health":{"timeout":"5 segundos"}}`,
		"health every con dorado": `{"kind":"http","snapshot":"s","contract":"openai","health":{"path":"/h","every":"10s"}}`,
		"health every 1ms":        `{"kind":"http","upstream":"http://h:1","contract":"openai","health":{"path":"/h","every":"1ms"}}`,
		"limits en chispa-decide": `{"kind":"http","snapshot":"s","contract":"chispa-decide","limits":{"max_tokens":5}}`,
		"limits negativos":        `{"kind":"http","snapshot":"s","contract":"openai","limits":{"n":-1}}`,
		"upstream_model en raw":   `{"kind":"http","snapshot":"s","contract":"raw","routes":["GET /a"],"upstream_model":"m"}`,
		"upstream_model con \\n":  `{"kind":"http","snapshot":"s","contract":"openai","upstream_model":"a\nb"}`,
		"auth sin header":         `{"kind":"http","upstream":"http://h:1","contract":"openai","auth":{"file":"/k"}}`,
		"auth header Host":        `{"kind":"http","upstream":"http://h:1","contract":"openai","auth":{"header":"Host","file":"/k"}}`,
		"auth header con espacio": `{"kind":"http","upstream":"http://h:1","contract":"openai","auth":{"header":"X Key","file":"/k"}}`,
		"auth sin file":           `{"kind":"http","upstream":"http://h:1","contract":"openai","auth":{"header":"X-Key"}}`,
		"auth relativa sin ruta":  `{"kind":"http","upstream":"http://h:1","contract":"openai","auth":{"header":"X-Key","file":"k"}}`,
		"tenant que no existe":    `{"kind":"http","snapshot":"s","contract":"openai","tenants":["otro"]}`,
		"tenant repetido":         `{"kind":"http","snapshot":"s","contract":"openai","tenants":["acme","acme"]}`,
		"http con path":           `{"kind":"http","snapshot":"s","contract":"openai","path":"x.chispa"}`,
		"http con backend":        `{"kind":"http","snapshot":"s","contract":"openai","backend":"microvm"}`,
		"von con port":            `{"kind":"von","snapshot":"s","port":8080}`,
		"von con contract":        `{"kind":"von","snapshot":"s","contract":"openai"}`,
		"von con upstream":        `{"kind":"von","snapshot":"s","upstream":"http://h:1"}`,
		"von con routes":          `{"kind":"von","snapshot":"s","routes":["POST /x"]}`,
		"von con auth":            `{"kind":"von","snapshot":"s","auth":{"header":"X-Key","file":"/k"}}`,
		"embed con limits":        `{"kind":"embed","snapshot":"s","limits":{"n":1}}`,
		"chispa en proceso idle":  `{"kind":"chispa","path":"x","idle":"remove"}`,
		"chispa microvm tenants":  `{"kind":"chispa","backend":"microvm","snapshot":"s","tenants":["acme"]}`,
	}
	if len(malas) < 12 {
		t.Fatal("the plan asks for at least 12 bad configurations")
	}
	// "/k" es un fichero de credencial válido: así los casos de auth fallan por
	// lo que dicen y no porque falte el fichero.
	clave := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(clave, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	conClave := func(m string) string { return strings.ReplaceAll(m, `"/k"`, strconv.Quote(clave)) }
	for nombre, m := range malas {
		src := `{"models":{"m":` + conClave(m) + `}` + ten + `}`
		_, err := ParseConfig(strings.NewReader(src))
		if err == nil {
			t.Errorf("%s: accepted %s", nombre, m)
			continue
		}
		t.Logf("%s: %v", nombre, err)
	}
	// Y lo que sí vale, para que la tabla no pase por rechazarlo todo.
	buenas := []string{
		`{"kind":"http","snapshot":"s","contract":"openai"}`,
		`{"kind":"http","snapshot":"s","contract":"openai-embeddings","port":65535}`,
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","tenants":["acme"]}`,
		`{"kind":"http","upstream":"https://h:8443/v1/","contract":"openai","health":{"path":"/health","every":"1s","timeout":"500ms","expect":204}}`,
		`{"kind":"http","snapshot":"s","contract":"raw","routes":["GET /","DELETE /x/","PUT /a/b-c_d.e"]}`,
		`{"kind":"von","snapshot":"s","idle":"freeze","max_inflight":2,"tenants":["acme"],"limits":{"max_tokens":64}}`,
		`{"kind":"embed","snapshot":"s","idle":"pause","health":{"path":"/ready"}}`,
		`{"kind":"chispa","backend":"microvm","snapshot":"s","idle":"remove","max_inflight":8}`,
		`{"kind":"http","upstream":"http://h:1","contract":"openai","auth":{"header":"X-Api-Key","file":"/k"}}`,
	}
	for _, m := range buenas {
		src := `{"models":{"m":` + conClave(m) + `}` + ten + `}`
		if _, err := ParseConfig(strings.NewReader(src)); err != nil {
			t.Errorf("rejected %s: %v", m, err)
		}
	}
}

// TestBackendAuthFichero: auth.file es relativo al ai.json, tiene que existir
// y no ser legible por otros (0600), como el ai.token.
func TestBackendAuthFichero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ai.json")
	escribe := func(modo os.FileMode) {
		k := filepath.Join(dir, "k")
		_ = os.Remove(k)
		if err := os.WriteFile(k, []byte("secreto"), modo); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(k, modo); err != nil {
			t.Fatal(err)
		}
	}
	src := `{"models":{"g":{"kind":"http","upstream":"http://h:1","contract":"openai","auth":{"header":"X-Key","file":"k"}}}}`
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Error("missing auth file: accepted")
	}
	escribe(0o644)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Errorf("auth file readable by others: %v", err)
	}
	escribe(0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Backend("g"); b == nil || b.Auth.File != filepath.Join(dir, "k") {
		t.Errorf("auth: %+v", b)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(src, `"file":"k"`, `"file":"d"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Error("auth file is a directory: accepted")
	}
}

// TestBackendDevuelveCopiaEstable: el backend se calcula una vez al validar y
// se comparte; Backends() da un slice nuevo cada vez (reordenarlo no afecta).
func TestBackendDevuelveCopiaEstable(t *testing.T) {
	c := parse(t, `{"models":{"a":{"kind":"von","snapshot":"s"},"b":{"kind":"embed","snapshot":"t"}}}`)
	if a1, a2 := c.Backend("a"), c.Backend("a"); a1 != a2 {
		t.Error("Backend should return the validated, shared value")
	}
	l := c.Backends()
	l[0], l[1] = l[1], l[0]
	if c.Backends()[0].Name != "a" {
		t.Error("Backends() shares its slice with the caller")
	}
}
