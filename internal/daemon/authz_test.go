package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// Uids de los tests. Ninguno es root ni el del proceso (que son admin
// implícitos): se comprueba en servidorAuthz.
const (
	uidAdmin   = 71001
	uidA       = 72001
	uidB       = 72002
	uidNadie   = 79999
	gidEquipoC = 73000
	uidDeC     = 72003 // sin regla propia: entra por el grupo
	tokenDeA   = "token-secreto-de-a"
)

func politicaDePrueba(t *testing.T) *Politica {
	t.Helper()
	h := sha256.Sum256([]byte(tokenDeA))
	js := fmt.Sprintf(`{
	  "rules": [
	    {"uid": %d, "role": "admin"},
	    {"uid": %d, "role": "tenant:a"},
	    {"user": "bea", "role": "tenant:b"},
	    {"group": "equipo-c", "role": "tenant:c"}
	  ],
	  "shared_templates": ["base", "base-de-b"],
	  "tokens": [{"sha256": %q, "role": "tenant:a"}]
	}`, uidAdmin, uidA, hex.EncodeToString(h[:]))
	res := resolutor{
		usuario: func(n string) (int, error) {
			if n == "bea" {
				return uidB, nil
			}
			return 0, errors.New("unknown user")
		},
		grupo: func(n string) (int, error) {
			if n == "equipo-c" {
				return gidEquipoC, nil
			}
			return 0, errors.New("unknown group")
		},
	}
	p, err := parsePolitica([]byte(js), res)
	if err != nil {
		t.Fatal(err)
	}
	p.Ruta = "/etc/kling/authz.json"
	// Grupos suplementarios de mentira (Linux los resuelve por la base de
	// usuarios).
	p.grupos = func(uid int) []int {
		if uid == uidDeC {
			return []int{gidEquipoC}
		}
		return nil
	}
	for _, u := range []int{uidAdmin, uidA, uidB, uidNadie, uidDeC} {
		if p.admins[u] {
			t.Fatalf("uid %d de prueba es admin implícito (¿el test corre con ese uid?)", u)
		}
	}
	return p
}

// sembrar escribe máquinas, snapshots y grafos de dos inquilinos (a y b) y de
// nadie (lo de un admin), antes de arrancar el Manager.
func sembrar(t *testing.T, root string) {
	t.Helper()
	etq := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	maquinas := []*api.Machine{
		{ID: "aaaa000000000001", Name: "caja-a", State: api.StateWarm, Labels: etq(api.LabelOwner, "a", api.LabelKind, api.KindSandbox)},
		{ID: "aaaa000000000002", Name: "svc-a", State: api.StateWarm, Labels: etq(api.LabelOwner, "a")},
		{ID: "bbbb000000000001", Name: "caja-b", State: api.StateWarm, Labels: etq(api.LabelOwner, "b", api.LabelKind, api.KindSandbox)},
		{ID: "dddd000000000001", Name: "del-admin", State: api.StateWarm},
	}
	b, _ := json.Marshal(maquinas)
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	snaps := map[string]map[string]string{
		"snap-a":    etq(api.LabelOwner, "a"),
		"snap-b":    etq(api.LabelOwner, "b"),
		"base":      etq(api.LabelDBOwner, "local"), // compartida, sin dueño
		"privada":   nil,                            // sin dueño y no compartida
		"base-de-b": etq(api.LabelOwner, "b"),       // en la lista, pero tiene dueño
	}
	for n, l := range snaps {
		dir := filepath.Join(root, "snapshots", n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta, _ := json.Marshal(api.Snapshot{Name: n, Image: "min", Labels: l})
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	grafos := []api.Graph{
		{ID: "ga00000000000001", Name: "grafo-a", Nodes: map[string]api.GraphNode{
			"x": {Image: "min", Labels: etq(api.LabelOwner, "a")}, "y": {Image: "min", Labels: etq(api.LabelOwner, "a")}}},
		{ID: "gb00000000000001", Name: "grafo-b", Nodes: map[string]api.GraphNode{"x": {Image: "min", Labels: etq(api.LabelOwner, "b")}}},
		// Mezclado: no es de nadie.
		{ID: "gm00000000000001", Name: "grafo-mixto", Nodes: map[string]api.GraphNode{
			"x": {Image: "min", Labels: etq(api.LabelOwner, "a")}, "y": {Image: "min"}}},
	}
	gdir := filepath.Join(root, "store", "graph")
	if err := os.MkdirAll(gdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, g := range grafos {
		b, _ := json.Marshal(g)
		if err := os.WriteFile(filepath.Join(gdir, g.ID+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func servidorAuthz(t *testing.T, pol *Politica) *Server {
	t.Helper()
	root := t.TempDir()
	sembrar(t, root)
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return &Server{mgr: mgr, root: root, bus: bus, store: &store{dir: filepath.Join(root, "store")}, authz: pol}
}

// como hace una petición con un peercred falso: el uid que el daemon vería al
// otro lado del socket. uid < 0 = no se pudo leer.
func como(t *testing.T, h http.Handler, uid int, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ll := Llamante{UID: uid, GID: uid, Conocido: uid >= 0}
	req = req.WithContext(context.WithValue(req.Context(), claveLlamante{}, ll))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// conStubs monta las rutas de verdad con su acción, pero con un handler que
// solo dice que se llegó: la decisión de autorización, sin microVMs. Devuelve
// en el cuerpo el {ref} con el que llegó y el cuerpo que le dejaron.
func conStubs(s *Server) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.rutas() {
		rt.h = func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			w.WriteHeader(299)
			fmt.Fprintf(w, "ref=%s\n%s", r.PathValue("ref"), b)
		}
		mux.HandleFunc(rt.patron, s.autorizar(rt))
	}
	return sinBarrasEscapadas(mux)
}

// rutaConcreta rellena el patrón con un recurso de cada tipo.
func rutaConcreta(patron, maquina, snap, grafo string) (method, path string) {
	method, path, _ = strings.Cut(patron, " ")
	r := strings.NewReplacer("{ref}", maquina, "{name}", snap, "{env}", "API_KEY", "{key}", "nota", "{ns}", "mcp", "{snap}", "s1")
	if strings.HasPrefix(path, "/graphs/") {
		r = strings.NewReplacer("{ref}", grafo)
	}
	if strings.HasPrefix(path, "/images/") || strings.HasPrefix(path, "/volumes/") {
		r = strings.NewReplacer("{name}", "min", "{snap}", "s1")
	}
	return method, r.Replace(path)
}

// cuerpoValido es un cuerpo que un inquilino puede mandar a cada ruta que lo
// revisa.
func cuerpoValido(patron string) string {
	switch patron {
	case "POST /machines", "POST /sandboxes":
		return `{"image":"min"}`
	case "POST /graphs":
		return `{"graph":{"name":"g","nodes":{"n":{"image":"min"}}}}`
	case "POST /machines/{ref}/commit":
		return `{"name":"nuevo"}`
	case "PUT /machines/{ref}/labels":
		return `{"color":"azul"}`
	case "POST /machines/{ref}/credentials", "PUT /snapshots/{name}/credentials":
		return `{"credentials":[]}`
	}
	return ""
}

// La tabla de acciones × roles: para cada ruta del daemon y cada rol, si
// llega al handler o no, sobre lo propio y sobre lo ajeno.
func TestAuthzAccionesPorRol(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := conStubs(s)

	llega := func(rr *httptest.ResponseRecorder) bool { return rr.Code == 299 }
	for _, rt := range s.rutas() {
		body := cuerpoValido(rt.patron)
		// Lo propio de a (y lo ajeno de b).
		mPropia, mAjena := "caja-a", "caja-b"
		if !strings.HasPrefix(rt.patron, "POST /sandboxes/") && !strings.HasPrefix(rt.patron, "GET /sandboxes/") &&
			!strings.HasPrefix(rt.patron, "DELETE /sandboxes/") {
			mPropia = "svc-a"
		}
		mp, pp := rutaConcreta(rt.patron, mPropia, "snap-a", "grafo-a")
		_, pa := rutaConcreta(rt.patron, mAjena, "snap-b", "grafo-b")
		_, pn := rutaConcreta(rt.patron, "del-admin", "privada", "grafo-mixto")

		// admin: todo llega, también lo ajeno y lo de nadie.
		for _, p := range []string{pp, pa, pn} {
			if rr := como(t, h, uidAdmin, mp, p, body); !llega(rr) {
				t.Errorf("admin %s %s = %d %s", mp, p, rr.Code, rr.Body)
			}
		}
		// Sin rol: solo /info.
		rr := como(t, h, uidNadie, mp, pp, body)
		if (rt.accion == AccionInfo) != llega(rr) {
			t.Errorf("sin rol %s %s = %d %s", mp, pp, rr.Code, rr.Body)
		}
		if rt.accion != AccionInfo && rr.Code != http.StatusForbidden {
			t.Errorf("sin rol %s %s = %d, quería 403", mp, pp, rr.Code)
		}
		// Sin credenciales legibles: como sin rol.
		if rr := como(t, h, -1, mp, pp, body); (rt.accion == AccionInfo) != llega(rr) {
			t.Errorf("desconocido %s %s = %d %s", mp, pp, rr.Code, rr.Body)
		}

		// Inquilino a.
		propio := como(t, h, uidA, mp, pp, body)
		ajeno := como(t, h, uidA, mp, pa, body)
		deNadie := como(t, h, uidA, mp, pn, body)
		switch rt.accion {
		case AccionAdmin:
			for _, rr := range []*httptest.ResponseRecorder{propio, ajeno, deNadie} {
				if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "admin role") {
					t.Errorf("inquilino %s = %d %s, quería 403", rt.patron, rr.Code, rr.Body)
				}
			}
		case AccionInfo, AccionListar, AccionCrear, AccionImagenes:
			if !llega(propio) {
				t.Errorf("inquilino %s = %d %s, quería llegar", rt.patron, propio.Code, propio.Body)
			}
		case AccionMaquina, AccionGrafo, AccionSnapLeer:
			if !llega(propio) {
				t.Errorf("inquilino %s %s = %d %s, quería llegar", mp, pp, propio.Code, propio.Body)
			}
			for _, rr := range []*httptest.ResponseRecorder{ajeno, deNadie} {
				if rr.Code != http.StatusNotFound {
					t.Errorf("inquilino %s sobre lo ajeno = %d %s, quería 404", rt.patron, rr.Code, rr.Body)
				}
			}
		case AccionSnapEscribir:
			if !llega(propio) {
				t.Errorf("inquilino %s %s = %d %s, quería llegar", mp, pp, propio.Code, propio.Body)
			}
			if ajeno.Code != http.StatusNotFound || deNadie.Code != http.StatusNotFound {
				t.Errorf("inquilino %s sobre lo ajeno = %d / %d", rt.patron, ajeno.Code, deNadie.Code)
			}
		default:
			t.Errorf("%s: acción %q sin caso en el test", rt.patron, rt.accion)
		}
		// Una máquina resuelta llega al handler por su ID exacto.
		if rt.accion == AccionMaquina && llega(propio) && !strings.Contains(propio.Body.String(), "ref=aaaa00000000000") {
			t.Errorf("%s: el handler no recibió el ID: %s", rt.patron, propio.Body)
		}
		if rt.accion == AccionGrafo && llega(propio) && !strings.Contains(propio.Body.String(), "ref=ga00000000000001") {
			t.Errorf("%s: el handler no recibió el ID del grafo: %s", rt.patron, propio.Body)
		}
		// Crear exige revisar el cuerpo: si no, un inquilino pondría el dueño
		// que quisiera.
		if rt.accion == AccionCrear && rt.revisar == nil {
			t.Errorf("%s crea sin revisar el cuerpo", rt.patron)
		}
	}

	// Las plantillas compartidas: se leen, no se tocan; y una con dueño no se
	// comparte aunque esté en la lista.
	if rr := como(t, h, uidA, "GET", "/snapshots/base", ""); rr.Code != 299 {
		t.Errorf("leer una compartida = %d", rr.Code)
	}
	if rr := como(t, h, uidA, "DELETE", "/snapshots/base", ""); rr.Code != http.StatusForbidden {
		t.Errorf("borrar una compartida = %d %s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidA, "GET", "/snapshots/base-de-b", ""); rr.Code != http.StatusNotFound {
		t.Errorf("una con dueño en la lista de compartidas = %d", rr.Code)
	}
	// Un sandbox sin dueño no se alcanza ni por su ID.
	if rr := como(t, h, uidA, "GET", "/machines/dddd000000000001", ""); rr.Code != http.StatusNotFound {
		t.Errorf("máquina sin dueño por ID = %d", rr.Code)
	}
	// Ni el inquilino b ve lo de a.
	if rr := como(t, h, uidB, "GET", "/machines/svc-a", ""); rr.Code != http.StatusNotFound {
		t.Errorf("b sobre svc-a = %d", rr.Code)
	}
}

// Con los handlers de verdad: un inquilino no ve ni toca lo de otro.
func TestAuthzInquilinoNoVeAjenos(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()

	nombres := func(rr *httptest.ResponseRecorder) []string {
		var l []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &l); err != nil {
			t.Fatalf("%d %s: %v", rr.Code, rr.Body, err)
		}
		var out []string
		for _, x := range l {
			out = append(out, x.Name)
		}
		slices.Sort(out)
		return out
	}
	casos := []struct {
		uid  int
		path string
		want []string
	}{
		{uidA, "/machines", []string{"caja-a", "svc-a"}},
		{uidB, "/machines", []string{"caja-b"}},
		{uidAdmin, "/machines", []string{"caja-a", "caja-b", "del-admin", "svc-a"}},
		{uidA, "/sandboxes", []string{"caja-a"}},
		{uidB, "/sandboxes", []string{"caja-b"}},
		{uidA, "/snapshots", []string{"base", "snap-a"}},
		{uidB, "/snapshots", []string{"base", "base-de-b", "snap-b"}},
		{uidA, "/graphs", []string{"grafo-a"}},
		{uidB, "/graphs", []string{"grafo-b"}},
		{uidAdmin, "/graphs", []string{"grafo-a", "grafo-b", "grafo-mixto"}},
		// El del grupo equipo-c no tiene nada todavía: lista vacía, no ajena.
		{uidDeC, "/machines", nil},
		{uidDeC, "/snapshots", []string{"base"}},
	}
	for _, c := range casos {
		rr := como(t, h, c.uid, "GET", c.path, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("uid %d GET %s = %d %s", c.uid, c.path, rr.Code, rr.Body)
		}
		if got := nombres(rr); !slices.Equal(got, c.want) {
			t.Errorf("uid %d GET %s = %v, quería %v", c.uid, c.path, got, c.want)
		}
	}

	// Tocar lo ajeno: 404, y sigue ahí.
	for _, p := range [][2]string{{"DELETE", "/machines/caja-b"}, {"DELETE", "/sandboxes/caja-b"},
		{"POST", "/machines/caja-b/stop"}, {"GET", "/machines/caja-b/logs"}, {"DELETE", "/snapshots/snap-b"},
		{"DELETE", "/graphs/grafo-b"}} {
		if rr := como(t, h, uidA, p[0], p[1], ""); rr.Code != http.StatusNotFound {
			t.Errorf("a %s %s = %d %s", p[0], p[1], rr.Code, rr.Body)
		}
	}
	if _, ok := s.mgr.Get("caja-b"); !ok {
		t.Fatal("caja-b desapareció")
	}
	if _, err := s.mgr.Snapshot("snap-b"); err != nil {
		t.Fatal("snap-b desapareció")
	}
	if _, err := s.mgr.Graph("grafo-b"); err != nil {
		t.Fatal("grafo-b desapareció")
	}
	// Lo propio sí (borrar un snapshot suyo).
	if rr := como(t, h, uidA, "DELETE", "/snapshots/snap-a", ""); rr.Code != http.StatusNoContent {
		t.Errorf("a borra snap-a = %d %s", rr.Code, rr.Body)
	}

	// /info: cuenta solo lo suyo y dice el rol.
	var info api.Info
	json.Unmarshal(como(t, h, uidA, "GET", "/info", "").Body.Bytes(), &info)
	if info.Machines != 2 || info.Authz == nil || !info.Authz.Enabled || info.Authz.Role != "tenant:a" ||
		info.Authz.UID == nil || *info.Authz.UID != uidA {
		t.Errorf("info de a: %d máquinas, %+v", info.Machines, info.Authz)
	}
	if info.Root == "" {
		t.Errorf("info de a sin root")
	}
	// Sin rol (o sin peercred): la versión, las capacidades y su authz, nada
	// del host (ni la raíz, ni las carpetas compartibles, ni el almacén).
	for _, uid := range []int{uidNadie, -1} {
		info = api.Info{}
		rr := como(t, h, uid, "GET", "/info", "")
		json.Unmarshal(rr.Body.Bytes(), &info)
		if rr.Code != 200 || info.Machines != 0 || info.Authz == nil || info.Authz.Role != "none" || info.Version == "" {
			t.Errorf("info sin rol (uid %d): %d %+v", uid, rr.Code, info.Authz)
		}
		if info.Root != "" || info.ShareRoots != nil || info.CoW != nil || info.EncryptedAtRest != nil ||
			strings.Contains(rr.Body.String(), s.root) {
			t.Errorf("info sin rol (uid %d) cuenta el host: %s", uid, rr.Body)
		}
	}
	info = api.Info{}
	json.Unmarshal(como(t, h, uidAdmin, "GET", "/info", "").Body.Bytes(), &info)
	if info.Machines != 4 || info.Authz.Role != "admin" {
		t.Errorf("info de admin: %d máquinas, %+v", info.Machines, info.Authz)
	}

	// Los eventos: solo los de sus máquinas.
	ra := httptest.NewRequest("GET", "/events", nil)
	ra = ra.WithContext(context.WithValue(ra.Context(), claveRol{}, Rol{Tenant: "a"}))
	for _, c := range []struct {
		ev   api.Event
		want bool
	}{
		{api.Event{ID: "aaaa000000000001"}, true},
		{api.Event{ID: "bbbb000000000001"}, false},
		{api.Event{ID: "dddd000000000001"}, false},
		{api.Event{ID: "aaaa"}, false}, // un prefijo no es el ID
		{api.Event{Name: "snap-a"}, false},
	} {
		if got := s.eventoVisible(ra, c.ev); got != c.want {
			t.Errorf("evento %+v visible = %v", c.ev, got)
		}
	}
}

// Sin política, todo como siempre: sin rol, sin filtros, sin revisar cuerpos.
func TestAuthzSinPoliticaTodoIgual(t *testing.T) {
	s := servidorAuthz(t, nil)
	h := conStubs(s)
	for _, rt := range s.rutas() {
		m, p := rutaConcreta(rt.patron, "caja-b", "snap-b", "grafo-b")
		// Ni siquiera hace falta saber quién llama.
		if rr := call(t, h, m, p, `{"labels":{"kling.owner":"quien-sea"}}`); rr.Code != 299 {
			t.Errorf("%s %s = %d %s", m, p, rr.Code, rr.Body)
		}
	}
	// El cuerpo llega intacto: sin política, kling.owner es una etiqueta más.
	rr := call(t, h, "POST", "/machines", `{"image":"min","labels":{"kling.owner":"x"}}`)
	if !strings.Contains(rr.Body.String(), `{"image":"min","labels":{"kling.owner":"x"}}`) {
		t.Errorf("el cuerpo cambió: %s", rr.Body)
	}
	hr := s.routes()
	var l []*api.Machine
	json.Unmarshal(call(t, hr, "GET", "/machines", "").Body.Bytes(), &l)
	if len(l) != 4 {
		t.Errorf("sin política se ven %d máquinas, quería 4", len(l))
	}
	var info api.Info
	json.Unmarshal(call(t, hr, "GET", "/info", "").Body.Bytes(), &info)
	if info.Authz == nil || info.Authz.Enabled || info.Authz.Role != "" || info.Machines != 4 {
		t.Errorf("info sin política: %+v", info.Authz)
	}
	// Un token sin política se ignora.
	req := httptest.NewRequest("GET", "/machines", nil)
	req.Header.Set("Authorization", "Bearer lo-que-sea")
	w := httptest.NewRecorder()
	hr.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("token sin política = %d", w.Code)
	}
}

// Lo que un inquilino manda al crear o cambiar algo: el dueño lo pone el
// daemon, y nada del cuerpo lo saca de lo suyo.
func TestAuthzCuerposDeInquilino(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := conStubs(s)

	casos := []struct {
		nombre, method, path, body string
		code                       int
		contiene                   []string
	}{
		{"run sella el dueño", "POST", "/machines", `{"image":"min"}`, 299, []string{`"kling.owner":"a"`}},
		{"run no fija kling.owner", "POST", "/machines", `{"image":"min","labels":{"kling.owner":"b"}}`, 403, []string{"set by the daemon"}},
		{"ni con clave repetida", "POST", "/machines", `{"image":"min","labels":{"x":"1"},"labels":{"kling.owner":"b"}}`, 403, nil},
		{"kling.db.owner ajeno", "POST", "/machines", `{"image":"min","labels":{"kling.db.owner":"b"}}`, 403, []string{"kling.db.owner"}},
		{"kling.db.owner propio", "POST", "/machines", `{"image":"min","labels":{"kling.db.owner":"a"}}`, 299, []string{`"kling.db.owner":"a"`}},
		{"volumen", "POST", "/machines", `{"image":"min","volume":"datos"}`, 403, []string{"volumes"}},
		{"volúmenes", "POST", "/sandboxes", `{"image":"min","volumes":[{"name":"datos"}]}`, 403, []string{"volumes"}},
		{"carpetas", "POST", "/machines", `{"image":"min","shares":[{"source":"/etc","mount":"/x","mode":"ro"}]}`, 403, []string{"shares"}},
		{"plantilla ajena", "POST", "/machines", `{"from":"snap-b"}`, 404, []string{"does not exist"}},
		{"plantilla sin compartir", "POST", "/sandboxes", `{"from":"privada"}`, 404, nil},
		{"plantilla propia", "POST", "/sandboxes", `{"from":"snap-a"}`, 299, []string{`"kling.owner":"a"`}},
		// La compartida trae kling.db.owner=local: la instancia queda ligada
		// al inquilino.
		{"plantilla compartida", "POST", "/machines", `{"from":"base"}`, 299, []string{`"kling.owner":"a"`, `"kling.db.owner":"a"`}},
		{"grafo sella cada nodo", "POST", "/graphs", `{"graph":{"name":"g","nodes":{"n":{"image":"min"},"m":{"from":"snap-a"}}}}`, 299,
			[]string{`"n":{"image":"min","labels":{"kling.owner":"a"}}`, `"m":{"from":"snap-a","labels":{"kling.owner":"a"}}`}},
		{"grafo con plantilla ajena", "POST", "/graphs", `{"graph":{"name":"g","nodes":{"n":{"from":"snap-b"}}}}`, 404, []string{"node n"}},
		{"grafo con kling.owner", "POST", "/graphs", `{"graph":{"name":"g","nodes":{"n":{"image":"min","labels":{"kling.owner":"b"}}}}}`, 403, nil},
		{"etiquetas: kling.owner", "PUT", "/machines/svc-a/labels", `{"kling.owner":"b"}`, 403, nil},
		{"etiquetas: kling.db.owner ajeno", "PUT", "/machines/svc-a/labels", `{"kling.db.owner":"b"}`, 403, nil},
		{"etiquetas propias", "PUT", "/machines/svc-a/labels", `{"color":"azul","kling.db.owner":""}`, 299, nil},
		{"fork sella el dueño", "POST", "/sandboxes/caja-a/fork", `{"count":2}`, 299, []string{`"kling.owner":"a"`}},
		{"fork no fija kling.owner", "POST", "/sandboxes/caja-a/fork", `{"labels":{"kling.owner":"b"}}`, 403, nil},
		// Un nombre que no ve: la misma respuesta con -replace o sin él, sea de
		// otro inquilino o de un admin, y sin decir de quién es.
		{"commit reemplaza ajeno", "POST", "/machines/svc-a/commit", `{"name":"snap-b","replace":true}`, 409, []string{`snapshot name \"snap-b\" is taken: pick another name`}},
		{"commit sobre ajeno", "POST", "/machines/svc-a/commit", `{"name":"snap-b"}`, 409, []string{`snapshot name \"snap-b\" is taken: pick another name`}},
		{"commit sobre privada de admin", "POST", "/machines/svc-a/commit", `{"name":"privada"}`, 409, []string{`snapshot name \"privada\" is taken: pick another name`}},
		{"commit reemplaza compartida", "POST", "/machines/svc-a/commit", `{"name":"base","replace":true}`, 403, []string{"shared template"}},
		{"commit sobre compartida", "POST", "/machines/svc-a/commit", `{"name":"base"}`, 403, []string{"shared template"}},
		{"commit con nombre libre", "POST", "/machines/svc-a/commit", `{"name":"nuevo-a"}`, 299, nil},
		{"commit reemplaza propio", "POST", "/machines/svc-a/commit", `{"name":"snap-a","replace":true}`, 299, nil},
		{"credencial a máquina ajena", "POST", "/machines/svc-a/credentials",
			`{"credentials":[{"domain":"db","env":"PG","secret":"x","upstream_machine":"bbbb000000000001","upstream_owner":"a"}]}`, 404, nil},
		{"credencial con dueño ajeno", "POST", "/machines/svc-a/credentials",
			`{"credentials":[{"domain":"db","env":"PG","secret":"x","upstream_machine":"aaaa000000000001","upstream_owner":"b"}]}`, 403, nil},
		{"credencial propia", "POST", "/machines/svc-a/credentials",
			`{"credentials":[{"domain":"db","env":"PG","secret":"x","upstream_machine":"aaaa000000000001","upstream_owner":"a"}]}`, 299, nil},
		// Las credenciales de una plantilla pasan por la misma revisión que
		// las de una máquina.
		{"plantilla: credencial a máquina ajena", "PUT", "/snapshots/snap-a/credentials",
			`{"credentials":[{"domain":"db","env":"PG","secret":"x","upstream_machine":"bbbb000000000001","upstream_owner":"a"}]}`, 404, nil},
		{"plantilla: credencial con dueño ajeno", "PUT", "/snapshots/snap-a/credentials",
			`{"credentials":[{"domain":"db","env":"PG","secret":"x","upstream_machine":"aaaa000000000001","upstream_owner":"b"}]}`, 403, nil},
		{"plantilla: credencial propia", "PUT", "/snapshots/snap-a/credentials",
			`{"credentials":[{"domain":"db","env":"PG","secret":"x"}],"clear":true}`, 299, []string{`"clear":true`}},
		{"JSON roto", "POST", "/machines", `{"image":`, 400, nil},
		{"cuerpo enorme", "POST", "/machines", `{"image":"` + strings.Repeat("x", authzMaxFichero) + `"}`, 413, nil},
	}
	for _, c := range casos {
		rr := como(t, h, uidA, c.method, c.path, c.body)
		if rr.Code != c.code {
			t.Errorf("%s: %d %s, quería %d", c.nombre, rr.Code, rr.Body, c.code)
			continue
		}
		for _, x := range c.contiene {
			if !strings.Contains(rr.Body.String(), x) {
				t.Errorf("%s: falta %s en %s", c.nombre, x, rr.Body)
			}
		}
	}
	// Un admin sí puede crear a nombre de un inquilino, y su cuerpo no se toca.
	rr := como(t, h, uidAdmin, "POST", "/machines", `{"image":"min","labels":{"kling.owner":"b"}}`)
	if rr.Code != 299 || !strings.Contains(rr.Body.String(), `"kling.owner":"b"`) {
		t.Errorf("admin a nombre de b: %d %s", rr.Code, rr.Body)
	}
}

// Los roles por grupo y los tokens de inquilino.
func TestAuthzGruposYTokens(t *testing.T) {
	p := politicaDePrueba(t)
	casos := []struct {
		ll   Llamante
		want string
	}{
		{Llamante{UID: uidA, Conocido: true}, "tenant:a"},
		{Llamante{UID: uidB, Conocido: true}, "tenant:b"},
		{Llamante{UID: uidAdmin, Conocido: true}, "admin"},
		{Llamante{UID: 0, Conocido: true}, "admin"},                                            // root, implícito
		{Llamante{UID: os.Geteuid(), Conocido: true}, "admin"},                                 // el usuario del daemon
		{Llamante{UID: uidDeC, GID: 100, Conocido: true}, "tenant:c"},                          // Linux: grupo suplementario
		{Llamante{UID: 74000, GID: gidEquipoC, Conocido: true}, "tenant:c"},                    // grupo primario
		{Llamante{UID: 74001, GID: 20, Groups: []int{gidEquipoC}, Conocido: true}, "tenant:c"}, // macOS: grupos de la conexión
		{Llamante{UID: 74002, GID: 20, Groups: []int{}, Conocido: true}, "none"},
		{Llamante{UID: uidA}, "none"}, // sin peercred no hay rol, aunque el uid case
	}
	for _, c := range casos {
		if got := p.rol(c.ll).String(); got != c.want {
			t.Errorf("%+v = %s, quería %s", c.ll, got, c.want)
		}
	}

	s := servidorAuthz(t, p)
	h := s.routes()
	conToken := func(uid int, tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/machines", nil)
		req = req.WithContext(context.WithValue(req.Context(), claveLlamante{}, Llamante{UID: uid, Conocido: uid >= 0}))
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	// Quien no tiene rol entra como a con su token.
	if rr := conToken(uidNadie, "Bearer "+tokenDeA); rr.Code != 200 || strings.Contains(rr.Body.String(), "caja-b") ||
		!strings.Contains(rr.Body.String(), "caja-a") {
		t.Errorf("token de a: %d %s", rr.Code, rr.Body)
	}
	// Y un admin con el token de a se queda en a: el token manda.
	if rr := conToken(uidAdmin, "Bearer "+tokenDeA); rr.Code != 200 || strings.Contains(rr.Body.String(), "del-admin") {
		t.Errorf("admin con token de a: %d %s", rr.Code, rr.Body)
	}
	for _, tok := range []string{"Bearer nada", "Basic abc", "Bearer "} {
		if rr := conToken(uidAdmin, tok); rr.Code != http.StatusUnauthorized {
			t.Errorf("%q = %d, quería 401", tok, rr.Code)
		}
	}
	// Un token inválido no abre /info aunque /info conteste sin rol.
	req := httptest.NewRequest("GET", "/info", nil)
	req.Header.Set("Authorization", "Bearer nada")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("/info con token inválido = %d", rr.Code)
	}
}

func TestParsePolitica(t *testing.T) {
	res := resolutor{
		usuario: func(string) (int, error) { return 0, errors.New("unknown user") },
		grupo:   func(string) (int, error) { return 0, errors.New("unknown group") },
	}
	malas := map[string]string{
		"campo desconocido":  `{"rulez":[]}`,
		"regla sin selector": `{"rules":[{"role":"admin"}]}`,
		"dos selectores":     `{"rules":[{"uid":1,"gid":2,"role":"admin"}]}`,
		"rol desconocido":    `{"rules":[{"uid":1,"role":"root"}]}`,
		"inquilino inválido": `{"rules":[{"uid":1,"role":"tenant:Mal Nombre"}]}`,
		"usuario que no hay": `{"rules":[{"user":"nadie","role":"admin"}]}`,
		"grupo que no hay":   `{"rules":[{"group":"nadie","role":"admin"}]}`,
		"uid negativo":       `{"rules":[{"uid":-1,"role":"admin"}]}`,
		"token admin":        `{"tokens":[{"sha256":"` + strings.Repeat("a", 64) + `","role":"admin"}]}`,
		"token corto":        `{"tokens":[{"sha256":"abc","role":"tenant:x"}]}`,
		"compartida rara":    `{"shared_templates":["../x"]}`,
		"basura detrás":      `{"rules":[]} {}`,
	}
	for n, js := range malas {
		if _, err := parsePolitica([]byte(js), res); err == nil {
			t.Errorf("%s: debía fallar", n)
		}
	}
	if p, err := parsePolitica([]byte(`{"rules":[]}`), res); err != nil || p.rol(Llamante{UID: 5, Conocido: true}).Valido() {
		t.Errorf("política vacía: %v", err)
	}
}

func TestCargarPolitica(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "authz.json")
	if err := os.WriteFile(ok, []byte(`{"rules":[{"uid":1234,"role":"tenant:x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := CargarPolitica(ok, true)
	if err != nil || p == nil || p.Ruta != ok || p.rol(Llamante{UID: 1234, Conocido: true}).Tenant != "x" {
		t.Fatalf("política buena: %v %+v", err, p)
	}
	// La ruta por defecto que no existe: sin política, sin error.
	if p, err := CargarPolitica(filepath.Join(dir, "no-hay.json"), false); p != nil || err != nil {
		t.Fatalf("por defecto sin fichero: %v %v", p, err)
	}
	// Pedida y ausente: error.
	if _, err := CargarPolitica(filepath.Join(dir, "no-hay.json"), true); err == nil {
		t.Fatal("una política pedida que no existe debía ser un error")
	}
	// Escribible por otros: error.
	abierta := filepath.Join(dir, "abierta.json")
	os.WriteFile(abierta, []byte(`{"rules":[]}`), 0o600)
	os.Chmod(abierta, 0o666)
	if _, err := CargarPolitica(abierta, false); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("escribible por otros: %v", err)
	}
	// Un enlace: error, aunque apunte a una buena.
	enlace := filepath.Join(dir, "enlace.json")
	if err := os.Symlink(ok, enlace); err != nil {
		t.Fatal(err)
	}
	if _, err := CargarPolitica(enlace, false); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("enlace: %v", err)
	}
	// Un enlace roto en la ruta por defecto tampoco es "sin política".
	roto := filepath.Join(dir, "roto.json")
	if err := os.Symlink(filepath.Join(dir, "no-hay.json"), roto); err != nil {
		t.Fatal(err)
	}
	if p, err := CargarPolitica(roto, false); p != nil || err == nil {
		t.Fatalf("enlace roto: %v %v", p, err)
	}
	// Se comprueba el descriptor abierto: un directorio o una FIFO no son un
	// fichero regular, y la FIFO no deja al daemon esperando a un escritor.
	if _, err := CargarPolitica(dir, false); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("directorio: %v", err)
	}
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	hecho := make(chan error, 1)
	go func() { _, err := CargarPolitica(fifo, false); hecho <- err }()
	select {
	case err := <-hecho:
		if err == nil || !strings.Contains(err.Error(), "regular") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CargarPolitica se quedó esperando en una FIFO")
	}
	// Mal escrita: error con la ruta.
	mala := filepath.Join(dir, "mala.json")
	os.WriteFile(mala, []byte(`{"rules":[{"uid":1,"role":"superuser"}]}`), 0o600)
	if _, err := CargarPolitica(mala, false); err == nil || !strings.Contains(err.Error(), mala) {
		t.Fatalf("mal escrita: %v", err)
	}
}

// El peercred de verdad: por un socket Unix, el daemon ve el uid de este
// proceso.
func TestCredencialesParReales(t *testing.T) {
	dir, err := os.MkdirTemp("", "kz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("unix", filepath.Join(dir, "s"))
		if err == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ll, err := credencialesPar(c)
	if err != nil {
		t.Fatal(err)
	}
	if !ll.Conocido || ll.UID != os.Geteuid() {
		t.Fatalf("peercred = %+v, quería uid %d", ll, os.Geteuid())
	}
	// Un net.Conn que no es Unix no tiene credenciales.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := credencialesPar(a); err == nil {
		t.Fatal("un pipe no debía tener peercred")
	}
}

// De punta a punta por un socket Unix, con el cliente de verdad y un
// peercred falso inyectado en el servidor.
func TestAuthzPorElSocket(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	// El uid que verá el servidor; -1 = el peercred falla. Atómico: lo leen
	// las goroutines del servidor.
	var uid atomic.Int64
	uid.Store(uidA)
	s.identificar = func(net.Conn) (Llamante, error) {
		u := int(uid.Load())
		if u < 0 {
			return Llamante{}, errors.New("sin peercred")
		}
		return Llamante{UID: u, Conocido: true}, nil
	}
	dir, err := os.MkdirTemp("", "kz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.routes(), ConnContext: s.conContexto}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	ctx := context.Background()
	l, err := api.NewClient("unix://" + sock).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 {
		t.Fatalf("a por el socket ve %d máquinas, quería 2", len(l))
	}
	if _, err := api.NewClient("unix://"+sock).Get(ctx, "caja-b"); err == nil {
		t.Fatal("a ve caja-b por el socket")
	}

	// Sin rol: 403 con el uid en el mensaje.
	uid.Store(uidNadie)
	_, err = api.NewClient("unix://" + sock).List(ctx)
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden || !strings.Contains(se.Message, "no role") {
		t.Fatalf("sin rol: %v", err)
	}
	// Con el token de a en el entorno del cliente, entra como a.
	t.Setenv(api.AuthzTokenEnv, tokenDeA)
	l, err = api.NewClient("unix://" + sock).List(ctx)
	if err != nil || len(l) != 2 {
		t.Fatalf("con token: %d %v", len(l), err)
	}

	// Si el peercred falla, con política no hay rol.
	uid.Store(-1)
	t.Setenv(api.AuthzTokenEnv, "")
	if _, err := api.NewClient("unix://" + sock).List(ctx); err == nil {
		t.Fatal("sin peercred debía negarse")
	}
}
