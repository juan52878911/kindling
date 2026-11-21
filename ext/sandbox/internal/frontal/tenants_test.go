package frontal

import (
	"bytes"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/inquilinos"
)

// reloj es un reloj manual para el almacén.
type reloj struct {
	mu sync.Mutex
	t  time.Time
}

func (r *reloj) ahora() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.t }
func (r *reloj) avanza(d time.Duration) {
	r.mu.Lock()
	r.t = r.t.Add(d)
	r.mu.Unlock()
}

type bancoTenants struct {
	srv   *httptest.Server
	s     *Servidor
	alm   *inquilinos.Almacen
	dir   string
	reloj *reloj
	logs  *bytes.Buffer
	f     *falso
}

// montarTenants arranca un frontal con alice (entorno, sin admin), ops
// (entorno, admin) y el almacén en dir ("" = uno nuevo).
func montarTenants(t *testing.T, dir string, f *falso, rl *reloj) *bancoTenants {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	if f == nil {
		f = nuevoFalso(t)
	}
	if rl == nil {
		rl = &reloj{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	alm, err := inquilinos.Abrir(dir, inquilinos.Opciones{Ahora: rl.ahora})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	s, err := Nuevo(Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": f.endpoint}),
		Almacen: alm,
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "ops", Token: tokenAdmin, Admin: true}},
		Log:     log.New(io.MultiWriter(logs, &logT{t}), "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	t.Cleanup(alm.Cerrar)
	return &bancoTenants{srv: srv, s: s, alm: alm, dir: dir, reloj: rl, logs: logs, f: f}
}

type claveResp struct {
	ID           string     `json:"id"`
	Prefijo      string     `json:"prefix"`
	Clave        string     `json:"key"`
	ViejaCaduca  *time.Time `json:"old_key_expires"`
	ViejaClaveID string     `json:"old_key_id"`
}

// tenantConClave crea el tenant name (con topes) y una clave, y la devuelve.
func (b *bancoTenants) tenantConClave(t *testing.T, name string, limites map[string]int) claveResp {
	t.Helper()
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants", map[string]any{"name": name, "limits": limites}), 201)
	resp := pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/"+name+"/keys", nil)
	esperaCodigo(t, resp, 201)
	k := decodifica[claveResp](t, resp)
	if !strings.HasPrefix(k.Clave, "ksk_") || !strings.HasPrefix(k.Clave, k.Prefijo) {
		t.Fatalf("clave rara: %+v", k)
	}
	return k
}

func codigoDe(t *testing.T, b *bancoTenants, token, metodo, ruta string) int {
	t.Helper()
	resp := pide(t, b.srv, token, metodo, ruta, nil)
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTenantsCrearYUsarClave(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	k := b.tenantConClave(t, "carol", nil)
	esperaCodigo(t, pide(t, b.srv, k.Clave, "GET", "/v1/sandboxes", nil), 200)
	// Crear lo mismo otra vez: 409; nombre inválido: 400.
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants", map[string]any{"name": "carol"}), 409)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants", map[string]any{"name": "a/b"}), 400)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants", map[string]any{"name": "x", "limits": map[string]int{"max_shells": -1}}), 400)
	// Clave mal formada o inventada: 401.
	for _, mala := range []string{"ksk_000000000000_xxxxxxxx", "ksk_", k.Clave + "x", k.Clave[:len(k.Clave)-1]} {
		if c := codigoDe(t, b, mala, "GET", "/v1/sandboxes"); c != 401 {
			t.Errorf("clave %q: %d, want 401", mala, c)
		}
	}
}

func TestTenantsRotar(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	k := b.tenantConClave(t, "carol", nil)
	resp := pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys/"+k.ID+"/rotate", map[string]any{"grace_minutes": 5})
	esperaCodigo(t, resp, 201)
	n := decodifica[claveResp](t, resp)
	if n.Clave == k.Clave || n.ViejaClaveID != k.ID || n.ViejaCaduca == nil {
		t.Fatalf("rotación: %+v", n)
	}
	// Durante la gracia valen las dos.
	b.reloj.avanza(4 * time.Minute)
	if c := codigoDe(t, b, k.Clave, "GET", "/v1/sandboxes"); c != 200 {
		t.Errorf("vieja en gracia: %d", c)
	}
	if c := codigoDe(t, b, n.Clave, "GET", "/v1/sandboxes"); c != 200 {
		t.Errorf("nueva: %d", c)
	}
	// Pasada la gracia, la vieja no; la nueva sí.
	b.reloj.avanza(2 * time.Minute)
	if c := codigoDe(t, b, k.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("vieja tras la gracia: %d, want 401", c)
	}
	if c := codigoDe(t, b, n.Clave, "GET", "/v1/sandboxes"); c != 200 {
		t.Errorf("nueva tras la gracia: %d", c)
	}
	// La vieja ya caducada no se puede rotar; una gracia fuera de rango, 400; sin gracia, 0 = ya.
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys/"+k.ID+"/rotate", nil), 404)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys/"+n.ID+"/rotate", map[string]any{"grace_minutes": 99999}), 400)
	resp = pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys/"+n.ID+"/rotate", map[string]any{"grace_minutes": 0})
	esperaCodigo(t, resp, 201)
	n2 := decodifica[claveResp](t, resp)
	if c := codigoDe(t, b, n.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("gracia 0: %d, want 401", c)
	}
	if c := codigoDe(t, b, n2.Clave, "GET", "/v1/sandboxes"); c != 200 {
		t.Errorf("tercera: %d", c)
	}
}

func TestTenantsRevocarEsInmediato(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	k := b.tenantConClave(t, "carol", nil)
	if c := codigoDe(t, b, k.Clave, "GET", "/v1/sandboxes"); c != 200 {
		t.Fatalf("antes: %d", c)
	}
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/carol/keys/"+k.ID, nil), 204)
	if c := codigoDe(t, b, k.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("tras revocar: %d, want 401", c)
	}
	// Revocar otra vez no falla; una clave que no existe, 404.
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/carol/keys/"+k.ID, nil), 204)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/carol/keys/nope", nil), 404)
	// Una clave con caducidad propia caduca sola.
	resp := pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys", map[string]any{"expires_at": b.reloj.ahora().Add(time.Hour).Format(time.RFC3339)})
	esperaCodigo(t, resp, 201)
	k2 := decodifica[claveResp](t, resp)
	b.reloj.avanza(2 * time.Hour)
	if c := codigoDe(t, b, k2.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("caducada: %d, want 401", c)
	}
	// Borrar el tenant mata sus claves.
	k3 := decodifica[claveResp](t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys", nil))
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/carol", nil), 204)
	if c := codigoDe(t, b, k3.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("tenant borrado: %d, want 401", c)
	}
}

func TestTenantsLaClaveNoSeFiltra(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	k := b.tenantConClave(t, "carol", map[string]int{"max_sandboxes": 3})
	n := decodifica[claveResp](t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys/"+k.ID+"/rotate", nil))
	// Usarla y fallar con otras genera logs; ninguno puede llevar claves.
	pide(t, b.srv, k.Clave, "GET", "/v1/sandboxes", nil).Body.Close()
	pide(t, b.srv, k.Clave+"x", "GET", "/v1/sandboxes", nil).Body.Close()
	// La clave es <prefijo>_<secreto>; el secreto va en base64url, que también
	// usa "_": se corta por el prefijo, no por el último "_" (que a veces deja
	// un trozo de uno o dos caracteres y el test fallaba por azar).
	secretos := []string{k.Clave, n.Clave, strings.TrimPrefix(k.Clave, k.Prefijo+"_"), strings.TrimPrefix(n.Clave, n.Prefijo+"_")}
	// Ninguna lectura de la API la repite.
	for _, ruta := range []string{"/v1/tenants", "/v1/tenants/carol"} {
		resp := pide(t, b.srv, tokenAdmin, "GET", ruta, nil)
		cuerpo, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Contains(cuerpo, []byte(k.Prefijo)) {
			t.Errorf("%s no enseña el prefijo: %s", ruta, cuerpo)
		}
		for _, sec := range secretos {
			if bytes.Contains(cuerpo, []byte(sec)) {
				t.Errorf("%s lleva una clave en claro", ruta)
			}
		}
		for _, campo := range []string{`"hash"`, `"salt"`, `"key"`} {
			if bytes.Contains(cuerpo, []byte(campo)) {
				t.Errorf("%s lleva %s", ruta, campo)
			}
		}
	}
	for _, sec := range secretos {
		if strings.Contains(b.logs.String(), sec) {
			t.Errorf("la clave está en los logs")
		}
	}
	// Ni en el fichero, que además es 0600.
	b.alm.Cerrar()
	ruta := filepath.Join(b.dir, inquilinos.Fichero)
	datos, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range secretos {
		if bytes.Contains(datos, []byte(sec)) {
			t.Errorf("la clave está en el fichero")
		}
	}
	if !bytes.Contains(datos, []byte(k.Prefijo)) {
		t.Errorf("el fichero no guarda el prefijo")
	}
	st, err := os.Stat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("permisos %v, want 0600", st.Mode().Perm())
	}
	// No quedan temporales.
	ents, _ := os.ReadDir(b.dir)
	if len(ents) != 1 {
		t.Errorf("el directorio de estado tiene %d ficheros, want 1", len(ents))
	}
}

func TestTenantsReinicio(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	k := b.tenantConClave(t, "carol", map[string]int{"max_sandboxes": 1})
	k2 := decodifica[claveResp](t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/carol/keys", nil))
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/carol/keys/"+k2.ID, nil), 204)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "PATCH", "/v1/tenants/carol", map[string]any{"admin": true}), 200)
	b.alm.Cerrar()
	b.srv.Close()

	// Otro gateway sobre el mismo directorio.
	b2 := montarTenants(t, b.dir, b.f, b.reloj)
	esperaCodigo(t, pide(t, b2.srv, k.Clave, "GET", "/v1/sandboxes", nil), 200)
	if c := codigoDe(t, b2, k2.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("la revocada resucitó: %d", c)
	}
	resp := pide(t, b2.srv, tokenAdmin, "GET", "/v1/tenants/carol", nil)
	esperaCodigo(t, resp, 200)
	v := decodifica[vistaTenant](t, resp)
	if !v.Admin || v.Topes.MaxSandboxes != 1 || len(v.Claves) != 2 || v.Claves[0].UltimoUso == nil || v.Claves[1].Revocada == nil {
		t.Errorf("estado tras reiniciar: %+v", v)
	}
	// Sin el almacén (sin -state-dir) esta clave no abre nada.
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}}})
	if err != nil {
		t.Fatal(err)
	}
	solo := httptest.NewServer(s)
	defer solo.Close()
	if c := codigoDe(t, &bancoTenants{srv: solo}, k.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("sin almacén: %d", c)
	}
}

func TestTenantsConviveConElEntorno(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	// Los del entorno siguen funcionando con su token.
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "GET", "/v1/sandboxes", nil), 200)
	resp := pide(t, b.srv, tokenAdmin, "GET", "/v1/tenants", nil)
	esperaCodigo(t, resp, 200)
	l := decodifica[struct{ Tenants []vistaTenant }](t, resp)
	vistos := map[string]vistaTenant{}
	for _, v := range l.Tenants {
		vistos[v.ID] = v
	}
	if a := vistos["alice"]; a.Origen != "env" || !a.SoloLeer || len(a.Claves) != 0 {
		t.Errorf("alice: %+v", a)
	}
	if o := vistos["ops"]; !o.Admin || !o.SoloLeer {
		t.Errorf("ops: %+v", o)
	}
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "GET", "/v1/tenants/alice", nil), 200)
	// Solo lectura: nada de cambiarlos, borrarlos, darles claves ni crear uno del mismo nombre.
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "PATCH", "/v1/tenants/alice", map[string]any{"admin": true}), 409)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/alice", nil), 409)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants/alice/keys", nil), 409)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "POST", "/v1/tenants", map[string]any{"name": "alice"}), 409)
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "GET", "/v1/tenants/nadie", nil), 404)
	// Un inquilino del almacén con el mismo nombre que uno del entorno impide arrancar.
	if _, err := b.alm.Crear("bob", false, inquilinos.Topes{}); err != nil {
		t.Fatal(err)
	}
	_, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Almacen: b.alm, Tenants: []Tenant{{Nombre: "bob", Token: tokenBob}}})
	if err == nil {
		t.Error("Nuevo acepta bob en el entorno y en el almacén")
	}
	// Solo almacén (sin entorno) también arranca si hay inquilinos.
	if _, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Almacen: b.alm}); err != nil {
		t.Errorf("solo almacén: %v", err)
	}
}

func TestTenantsSoloAdmin(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	k := b.tenantConClave(t, "carol", nil)
	rutas := [][2]string{
		{"GET", "/v1/tenants"}, {"POST", "/v1/tenants"}, {"GET", "/v1/tenants/carol"},
		{"PATCH", "/v1/tenants/carol"}, {"DELETE", "/v1/tenants/carol"},
		{"POST", "/v1/tenants/carol/keys"}, {"POST", "/v1/tenants/carol/keys/" + k.ID + "/rotate"},
		{"DELETE", "/v1/tenants/carol/keys/" + k.ID},
	}
	for _, tok := range []string{tokenAlice, k.Clave} { // del entorno sin admin y del almacén sin admin
		for _, r := range rutas {
			if c := codigoDe(t, b, tok, r[0], r[1]); c != 403 {
				t.Errorf("%s %s con un no-admin: %d, want 403", r[0], r[1], c)
			}
		}
	}
	for _, r := range rutas {
		if c := codigoDe(t, b, "", r[0], r[1]); c != 401 {
			t.Errorf("%s %s sin token: %d, want 401", r[0], r[1], c)
		}
	}
	// Sigue vivo tras tanto intento.
	if c := codigoDe(t, b, k.Clave, "GET", "/v1/sandboxes"); c != 200 {
		t.Errorf("carol: %d", c)
	}
	// Un tenant del almacén con admin puede administrar, pero no quitarse el rol ni borrarse.
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "PATCH", "/v1/tenants/carol", map[string]any{"admin": true}), 200)
	esperaCodigo(t, pide(t, b.srv, k.Clave, "GET", "/v1/tenants", nil), 200)
	esperaCodigo(t, pide(t, b.srv, k.Clave, "PATCH", "/v1/tenants/carol", map[string]any{"admin": false}), 409)
	esperaCodigo(t, pide(t, b.srv, k.Clave, "DELETE", "/v1/tenants/carol", nil), 409)
	esperaCodigo(t, pide(t, b.srv, k.Clave, "GET", "/v1/hosts", nil), 200)
}

func TestTenantsSinAlmacen(t *testing.T) {
	srv, _ := montar(t, nil, Tenant{Nombre: "ops", Token: tokenAdmin, Admin: true})
	resp := pide(t, srv, tokenAdmin, "GET", "/v1/tenants", nil)
	esperaCodigo(t, resp, 200)
	esperaCodigo(t, pide(t, srv, tokenAdmin, "POST", "/v1/tenants", map[string]any{"name": "x"}), 501)
}

// Las cuentas del frontal se aplican igual a un tenant del almacén.
func TestTenantsAplicanTopes(t *testing.T) {
	f := nuevoFalso(t)
	b := montarTenants(t, "", f, nil)
	k := b.tenantConClave(t, "carol", map[string]int{"max_sandboxes": 1, "max_mem_mib": 256})
	crea(t, b.srv, k.Clave, CrearPeticion{Image: "base"})
	esperaCodigo(t, pide(t, b.srv, k.Clave, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 429)
	// Tope de memoria por sandbox del tenant.
	esperaCodigo(t, pide(t, b.srv, k.Clave, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", MemMiB: 512}), 400)
	// Cambiar los topes por la API vale desde la petición siguiente.
	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "PATCH", "/v1/tenants/carol", map[string]any{"limits": map[string]int{"max_sandboxes": 2}}), 200)
	crea(t, b.srv, k.Clave, CrearPeticion{Image: "base"})
	// Los sandboxes de carol no son de alice.
	resp := pide(t, b.srv, tokenAlice, "GET", "/v1/sandboxes", nil)
	esperaCodigo(t, resp, 200)
}
