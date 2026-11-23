package frontal

import (
	"net/url"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func idsDe(ss []Sandbox) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// C15: filtros por estado y etiqueta, y paginación por cursor.
func TestListarFiltraYPagina(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	var ids []string
	for i := 0; i < 5; i++ {
		lab := map[string]string{"grupo": "x"}
		if i%2 == 1 {
			lab = map[string]string{"grupo": "y", "rol": "web"}
		}
		ids = append(ids, crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Labels: lab}).ID)
	}
	f.mu.Lock()
	for _, mc := range f.maquinas {
		if "a/"+mc.ID == ids[0] {
			mc.State = api.StateStopped
		}
	}
	f.mu.Unlock()

	lista := func(q string) ([]Sandbox, string) {
		resp := pide(t, srv, tokenAlice, "GET", "/v1/sandboxes"+q, nil)
		esperaCodigo(t, resp, 200)
		next := resp.Header.Get("X-Kindling-Next-Cursor")
		return decodifica[[]Sandbox](t, resp), next
	}
	if todos, next := lista(""); len(todos) != 5 || next != "" {
		t.Fatalf("sin filtros: %d, cursor %q", len(todos), next)
	}
	if ss, _ := lista("?state=" + string(api.StateStopped)); len(ss) != 1 || ss[0].ID != ids[0] {
		t.Fatalf("state: %v", idsDe(ss))
	}
	if ss, _ := lista("?label=" + url.QueryEscape("grupo=y")); len(ss) != 2 {
		t.Fatalf("label: %v", idsDe(ss))
	}
	if ss, _ := lista("?label=grupo%3Dy&label=rol%3Dweb&state=running"); len(ss) != 2 {
		t.Fatalf("labels+state: %v", idsDe(ss))
	}
	if ss, _ := lista("?label=grupo%3Dz"); len(ss) != 0 {
		t.Fatalf("label sin casar: %v", idsDe(ss))
	}
	// Páginas de 2 hasta agotar: cada id una vez, el cursor se acaba al final.
	var vistos []string
	cursor := ""
	for pag := 0; pag < 5; pag++ {
		q := "?limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		ss, next := lista(q)
		if len(ss) > 2 {
			t.Fatalf("página de %d con limit=2", len(ss))
		}
		vistos = append(vistos, idsDe(ss)...)
		if next == "" {
			break
		}
		cursor = next
	}
	seen := map[string]bool{}
	for _, id := range vistos {
		if seen[id] {
			t.Fatalf("id repetido %s en %v", id, vistos)
		}
		seen[id] = true
	}
	if len(vistos) != 5 {
		t.Fatalf("paginando: %v", vistos)
	}
	for _, malo := range []string{"?limit=0", "?limit=-1", "?limit=abc", "?limit=100000", "?label=sinigual", "?state=raro"} {
		resp := pide(t, srv, tokenAlice, "GET", "/v1/sandboxes"+malo, nil)
		if resp.StatusCode != 400 {
			t.Errorf("%s = %d, quería 400", malo, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// C16: env al crear se guarda como sesión por defecto.
func TestCrearConEnvCreaLaSesionPorDefecto(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Env: map[string]string{"B": "2", "A": "1"}})
	f.procs.mu.Lock()
	req := f.procs.ultimaSe
	f.procs.mu.Unlock()
	if req.ID != "default" || len(req.Env) != 2 || req.Env[0] != "A=1" || req.Env[1] != "B=2" {
		t.Fatalf("sesión creada: %+v", req)
	}
	if req.TTLSeconds != api.SessionMaxTTL {
		t.Fatalf("la sesión por defecto caduca en %ds", req.TTLSeconds)
	}
	for _, malo := range []map[string]string{{"1X": "v"}, {"A=B": "v"}, {"": "v"}} {
		resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Env: malo})
		esperaCodigo(t, resp, 400)
		resp.Body.Close()
	}
}
