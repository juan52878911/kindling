package aigw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// replicasPorDorado es un Replicas de mentira que manda cada dorado a su
// servidor y apunta qué dorados se pidieron.
type replicasPorDorado struct {
	addr  map[string]string
	mu    sync.Mutex
	pedid []string
}

func (p *replicasPorDorado) Acquire(ctx context.Context, snap string) (*Replica, error) {
	return p.AcquirePort(ctx, snap, DefaultBackendPort)
}

func (p *replicasPorDorado) AcquirePort(_ context.Context, snap string, _ int) (*Replica, error) {
	p.mu.Lock()
	p.pedid = append(p.pedid, snap)
	p.mu.Unlock()
	return &Replica{Addr: p.addr[snap], Release: func() {}, Drop: func() {}}, nil
}

// servidorQueDice contesta siempre texto en JSON.
func servidorQueDice(t *testing.T, texto string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"de":"` + texto + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bloqueInquilino es el bloque chispa-decide de un inquilino bajo el alias
// chispa, con su propio dorado.
func bloqueInquilino(nombre, inquilino string) *Backend {
	b := decideBackend()
	b.Name, b.Snapshot, b.Tenants, b.Alias = nombre, nombre+"-v1", []string{inquilino}, "chispa"
	return b
}

// pasarelaAlias: dos inquilinos (acme, otro), cada uno con su bloque bajo el
// alias chispa y su servidor.
func pasarelaAlias(t *testing.T) (*Gateway, http.Handler, *replicasPorDorado) {
	t.Helper()
	sa, so := servidorQueDice(t, "acme"), servidorQueDice(t, "otro")
	g, h, _ := pasarela(t, sa, bloqueInquilino("chispa-acme", "acme"), bloqueInquilino("chispa-otro", "otro"))
	reps := &replicasPorDorado{addr: map[string]string{
		"chispa-acme-v1": strings.TrimPrefix(sa.URL, "http://"),
		"chispa-otro-v1": strings.TrimPrefix(so.URL, "http://"),
	}}
	g.replicas = reps
	return g, h, reps
}

// TestAliasResuelvePorInquilino: con el mismo alias, cada token llega al
// dorado de su inquilino; el nombre concreto sigue valiendo y conserva el 403.
func TestAliasResuelvePorInquilino(t *testing.T) {
	_, h, reps := pasarelaAlias(t)
	for tok, quiere := range map[string]string{tokAcme: "acme", tokOtro: "otro"} {
		rec := peticion(h, "POST", "/v1/m/chispa/v1/decide", tok, []byte(`{}`), nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"de":"`+quiere+`"`) {
			t.Fatalf("%s through the alias: %d %s", quiere, rec.Code, rec.Body)
		}
	}
	if got := strings.Join(reps.pedid, ","); got != "chispa-acme-v1,chispa-otro-v1" && got != "chispa-otro-v1,chispa-acme-v1" {
		t.Fatalf("goldens asked: %s", got)
	}
	if rec := peticion(h, "POST", "/v1/m/chispa-acme/v1/decide", tokAcme, []byte(`{}`), nil); rec.Code != 200 {
		t.Fatalf("by its own name: %d", rec.Code)
	}
	if rec := peticion(h, "POST", "/v1/m/chispa-acme/v1/decide", tokOtro, []byte(`{}`), nil); rec.Code != 403 {
		t.Fatalf("another tenant's block by name: %d", rec.Code)
	}
}

// TestAliasSinBloque404: un inquilino sin bloque propio bajo el alias recibe
// el mismo 404 que un modelo que no existe (no se revela que el alias lo usan
// otros).
func TestAliasSinBloque404(t *testing.T) {
	sa := servidorQueDice(t, "acme")
	g, h, _ := pasarela(t, sa, bloqueInquilino("chispa-acme", "acme"))
	reps := &replicasPorDorado{addr: map[string]string{"chispa-acme-v1": strings.TrimPrefix(sa.URL, "http://")}}
	g.replicas = reps
	alias := peticion(h, "POST", "/v1/m/chispa/v1/decide", tokOtro, []byte(`{}`), nil)
	nada := peticion(h, "POST", "/v1/m/no-existe/v1/decide", tokOtro, []byte(`{}`), nil)
	if alias.Code != 404 || nada.Code != 404 {
		t.Fatalf("alias without a block: %d, unknown model: %d", alias.Code, nada.Code)
	}
	if strings.ReplaceAll(alias.Body.String(), "chispa", "no-existe") != nada.Body.String() {
		t.Errorf("the 404s differ:\n%s\n%s", alias.Body, nada.Body)
	}
	if len(reps.pedid) != 0 {
		t.Fatalf("a replica was asked for: %v", reps.pedid)
	}
}

// TestAliasTokenPrincipal400: el token principal no es de ningún inquilino:
// con un alias no hay a qué bloque mandarlo, y se le pide el nombre concreto.
func TestAliasTokenPrincipal400(t *testing.T) {
	_, h, reps := pasarelaAlias(t)
	rec := peticion(h, "POST", "/v1/m/chispa/v1/decide", tokPrincipal, []byte(`{}`), nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "chispa-acme") {
		t.Fatalf("main token with an alias: %d %s", rec.Code, rec.Body)
	}
	if len(reps.pedid) != 0 {
		t.Fatalf("a replica was asked for: %v", reps.pedid)
	}
}

// Las métricas cuentan el modelo concreto, no el alias: un alias no abre
// series propias.
func TestAliasMetricasDelModeloConcreto(t *testing.T) {
	g, h, _ := pasarelaAlias(t)
	peticion(h, "POST", "/v1/m/chispa/v1/decide", tokAcme, []byte(`{}`), nil)
	var b strings.Builder
	g.met.write(&b, chispaStats{}, nil, nil, learnSnap{})
	if !strings.Contains(b.String(), `model="chispa-acme"`) || strings.Contains(b.String(), `model="chispa"`) {
		t.Fatalf("metrics:\n%s", grepLines(b.String(), "chispa"))
	}
}

// BenchmarkAliasResuelve mide el sobrecoste de llegar por el alias frente al
// nombre concreto, con 8 inquilinos bajo el mismo alias y el token del
// último (el peor caso de la búsqueda lineal entre los bloques del alias).
func BenchmarkAliasResuelve(b *testing.B) {
	srv := sumidero(b)
	var tenants []TenantConfig
	var bs []*Backend
	for i := 0; i < 8; i++ {
		n := string(rune('a' + i))
		tenants = append(tenants, TenantConfig{Name: "t" + n, Token: "token-del-inquilino-" + n})
		bs = append(bs, bloqueInquilino("chispa-t"+n, "t"+n))
	}
	g, err := New(Options{
		Config:   &Config{Models: map[string]*ModelConfig{"smol": {Kind: KindVON, Snapshot: "von-smol"}}, Tenants: tenants},
		Replicas: &puertoReplicas{addr: strings.TrimPrefix(srv.URL, "http://")},
		EvalDir:  b.TempDir(),
	})
	if err != nil {
		b.Fatal(err)
	}
	conBackends(g, bs...)
	h := g.Handler(tokPrincipal)
	cuerpo := []byte(`{"state":{},"questions":[]}`)
	for _, c := range []struct{ nombre, ruta string }{
		{"nombre", "/v1/m/chispa-th/v1/decide"},
		{"alias", "/v1/m/chispa/v1/decide"},
	} {
		b.Run(c.nombre, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				rec := peticion(h, "POST", c.ruta, "token-del-inquilino-h", cuerpo, nil)
				if rec.Code != 200 {
					b.Fatalf("%s: %d %s", c.ruta, rec.Code, rec.Body)
				}
			}
		})
	}
}

const dosInquilinos =`,"tenants":[{"name":"acme","token":"0123456789abcdef"},{"name":"globex","token":"fedcba9876543210"}]`

// TestChispaDecideExigeInquilino: con inquilinos declarados, un chispa-decide
// con dorado sirve a exactamente uno (sus bancos van en el dorado), salvo
// shared: true. Sin inquilinos declarados no cambia nada.
func TestChispaDecideExigeInquilino(t *testing.T) {
	for _, m := range []string{
		`{"kind":"http","snapshot":"s","contract":"chispa-decide"}`,
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","tenants":["acme","globex"]}`,
	} {
		_, err := cargaBloque(m, dosInquilinos)
		if err == nil || !strings.Contains(err.Error(), "shared") {
			t.Errorf("accepted %s: %v", m, err)
		}
	}
	for _, m := range []string{
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","tenants":["acme"]}`,
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","shared":true}`,
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","tenants":["acme","globex"],"shared":true}`,
		`{"kind":"http","upstream":"http://h:1","contract":"chispa-decide"}`,
		`{"kind":"http","snapshot":"s","contract":"openai","tenants":["acme","globex"]}`,
	} {
		if _, err := cargaBloque(m, dosInquilinos); err != nil {
			t.Errorf("rejected %s: %v", m, err)
		}
	}
	if _, err := cargaBloque(`{"kind":"http","snapshot":"s","contract":"chispa-decide"}`, ""); err != nil {
		t.Errorf("without tenants the rule does not apply: %v", err)
	}
	for _, m := range []string{
		`{"kind":"http","snapshot":"s","contract":"openai","shared":true}`,
		`{"kind":"von","snapshot":"s","shared":true}`,
	} {
		if _, err := cargaBloque(m, dosInquilinos); err == nil {
			t.Errorf("shared outside chispa-decide accepted: %s", m)
		}
	}
}

// TestAliasValidacion: un inquilino aparece una vez por alias, el alias no
// pisa el nombre de un modelo, dos bloques del mismo alias no comparten
// dorado, y todo bloque con alias dice a qué inquilinos sirve.
func TestAliasValidacion(t *testing.T) {
	bloque := func(snap, tenants string) string {
		return `{"kind":"http","snapshot":"` + snap + `","contract":"chispa-decide","alias":"chispa","tenants":[` + tenants + `]}`
	}
	malas := map[string]string{
		"inquilino repetido": `"a":` + bloque("sa", `"acme"`) + `,"b":` + bloque("sb", `"acme"`),
		"alias = modelo":     `"chispa":` + bloque("sa", `"acme"`) + `,"b":` + bloque("sb", `"globex"`),
		"dorado compartido":  `"a":` + bloque("s", `"acme"`) + `,"b":` + bloque("s", `"globex"`),
		"sin inquilinos":     `"a":{"kind":"http","snapshot":"s","contract":"chispa-decide","alias":"chispa","shared":true}`,
		"alias inválido":     `"a":{"kind":"http","snapshot":"s","contract":"chispa-decide","alias":"Chispa!","tenants":["acme"]}`,
		"alias en openai":    `"a":{"kind":"http","snapshot":"s","contract":"openai","alias":"chispa","tenants":["acme"]}`,
		"alias en von":       `"a":{"kind":"von","snapshot":"s","alias":"chispa","tenants":["acme"]}`,
		"dorado sin alias":   `"a":` + bloque("s", `"acme"`) + `,"b":{"kind":"http","snapshot":"s","contract":"chispa-decide","tenants":["globex"]}`,
	}
	for nombre, ms := range malas {
		if _, err := ParseConfig(strings.NewReader(`{"models":{` + ms + `}` + dosInquilinos + `}`)); err == nil {
			t.Errorf("%s: accepted", nombre)
		} else {
			t.Logf("%s: %v", nombre, err)
		}
	}
	c, err := ParseConfig(strings.NewReader(`{"models":{"chispa-acme":` + bloque("sa", `"acme"`) + `,"chispa-globex":` + bloque("sb", `"globex"`) + `}` + dosInquilinos + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Backend("chispa-globex"); b.Alias != "chispa" || b.Tenants[0] != "globex" {
		t.Fatalf("backend %+v", b)
	}
}
