package machine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// ObsoletosEnDisco es lo que `kling upgrade` mira antes de parar nada: lo que
// el daemon nuevo se negaría a leer. Cada caso prepara un root y dice cuántos
// avisos espera y qué tiene que decir cada uno.
func TestObsoletosEnDisco(t *testing.T) {
	casos := []struct {
		nombre string
		prep   func(root string)
		quiero []string // un fragmento por aviso, en orden
	}{
		{"root vacío", func(string) {}, nil},
		{"root que no existe", func(root string) { os.Remove(root) }, nil},
		{"state.json de v0.13 con warm", func(root string) {
			escribir(t, filepath.Join(root, "state.json"), `[{"id":"aaaa","state":"running"},{"id":"bbbb","state":"warm"}]`)
		}, []string{"v0.13"}},
		{"state.json v0 sin warm", func(root string) {
			escribir(t, filepath.Join(root, "state.json"), `[{"id":"aaaa","state":"frozen"}]`)
		}, nil},
		// "warm" solo cuenta en el formato de antes (un array, esquema 0).
		{"warm en un esquema actual", func(root string) {
			escribir(t, filepath.Join(root, "state.json"), `{"schema":1,"machines":[{"id":"aaaa","state":"warm"}]}`)
		}, nil},
		// Uno más nuevo no es obsoleto: eso lo dice Incompatibles.
		{"state.json más nuevo", func(root string) {
			escribir(t, filepath.Join(root, "state.json"), `{"schema":9999}`)
		}, nil},
		{"state.json ilegible", func(root string) {
			escribir(t, filepath.Join(root, "state.json"), `[{"id":`)
		}, nil},
		{"state.json con un schema que no es un número", func(root string) {
			escribir(t, filepath.Join(root, "state.json"), `{"schema":"dos"}`)
		}, nil},
		{"meta.json de v0.4", func(root string) {
			escribir(t, filepath.Join(root, "snapshots", "viejo", "meta.json"), `{"name":"viejo","tools":[]}`)
			escribir(t, filepath.Join(root, "snapshots", "nuevo", "meta.json"), `{"schema":1,"name":"nuevo"}`)
		}, []string{"snapshots/viejo/meta.json was written by kling v0.4"}},
		{"meta.json de v0.4 por cada campo", func(root string) {
			for _, k := range clavesV04 {
				escribir(t, filepath.Join(root, "snapshots", "p-"+k, "meta.json"), `{"`+k+`":null}`)
			}
		}, []string{"v0.4", "v0.4", "v0.4", "v0.4", "v0.4"}},
		// Los campos de MCP en un meta con esquema son claves desconocidas que
		// se conservan, no un meta de v0.4.
		{"campo de MCP en un meta con esquema", func(root string) {
			escribir(t, filepath.Join(root, "snapshots", "p", "meta.json"), `{"schema":1,"tools":[]}`)
		}, nil},
		{"meta.json ilegible", func(root string) {
			escribir(t, filepath.Join(root, "snapshots", "p", "meta.json"), `{"tools":`)
		}, nil},
		// Un meta.json fuera de snapshots/<nombre>/ no es de ningún dorado.
		{"meta.json fuera de sitio", func(root string) {
			escribir(t, filepath.Join(root, "snapshots", "a", "b", "meta.json"), `{"tools":[]}`)
			escribir(t, filepath.Join(root, "meta.json"), `{"tools":[]}`)
		}, nil},
		{"links.json de v0.4", func(root string) {
			escribir(t, filepath.Join(root, "links.json"), `{}`)
		}, []string{"links.json holds MCP links from kling v0.4"}},
		{"links.json ya migrado", func(root string) {
			escribir(t, filepath.Join(root, "links.json"), `{}`)
			escribir(t, filepath.Join(root, "store", "mcp", "links.json"), `{}`)
		}, nil},
		{"todo a la vez", func(root string) {
			escribir(t, filepath.Join(root, "state.json"), `[{"id":"aaaa","state":"warm"}]`)
			escribir(t, filepath.Join(root, "snapshots", "viejo", "meta.json"), `{"health":"ok"}`)
			escribir(t, filepath.Join(root, "links.json"), `{}`)
		}, []string{"state.json", "meta.json", "links.json"}},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "kindling")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			c.prep(root)
			got, err := ObsoletosEnDisco(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.quiero) {
				t.Fatalf("avisos = %q, quiero %d", got, len(c.quiero))
			}
			for i, frag := range c.quiero {
				if !strings.Contains(got[i], frag) {
					t.Errorf("aviso %d = %q, sin %q", i, got[i], frag)
				}
			}
		})
	}
}

// Un meta.json que existe y no se puede leer es un error, no «nada
// obsoleto»: si no, upgrade pararía el daemon y el nuevo no arrancaría.
func TestObsoletosEnDiscoMetaQueNoSeLee(t *testing.T) {
	root := t.TempDir()
	// Un directorio llamado meta.json: el glob lo encuentra y leerlo falla
	// (también como root, a diferencia de quitar permisos).
	if err := os.MkdirAll(filepath.Join(root, "snapshots", "p", "meta.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := ObsoletosEnDisco(root); err == nil {
		t.Fatalf("sin error; avisos = %q", got)
	}
}

func TestNombreFork(t *testing.T) {
	for _, original := range []string{
		"a",
		"tienda",
		"exactamente-quince", // 18: se recorta
		"abcdefghijklmno",    // 15 justos: no se recorta
		"abcdefghijklmn-pqr", // el corte deja un guion al final
		"abcdefghijklm---xyz",
		"abcdefghijklmnopqrstuvwx", // 24, el máximo
	} {
		got := nombreFork(original)
		if err := api.ValidateGraphName(got); err != nil {
			t.Errorf("nombreFork(%q) = %q no es un nombre válido: %v", original, got, err)
		}
		base, sufijo, ok := strings.Cut(got[max(0, len(got)-9):], "-")
		if !ok || base != "" || len(sufijo) != 8 {
			t.Errorf("nombreFork(%q) = %q: quiero <prefijo>-<8 hex>", original, got)
		}
		prefijo := got[:len(got)-9]
		if !strings.HasPrefix(original, prefijo) || strings.HasSuffix(prefijo, "-") {
			t.Errorf("nombreFork(%q) = %q: prefijo %q", original, got, prefijo)
		}
		if len(original) <= 15 && prefijo != original {
			t.Errorf("nombreFork(%q) = %q: recortó un nombre que cabía", original, got)
		}
	}
	// Dos forks del mismo grafo no se llaman igual.
	if a, b := nombreFork("tienda"), nombreFork("tienda"); a == b {
		t.Errorf("dos forks con el mismo nombre %q", a)
	}
}

// Un volumen en escritura en cualquier nodo, con máquina o sin ella (un lazy
// que aún no arrancó), impide el fork; en solo lectura se comparte.
func TestVolumenesDeFork(t *testing.T) {
	rw := []api.VolumeAttachment{{Name: "datos", Mount: "/datos"}}
	ro := []api.VolumeAttachment{{Name: "datos", Mount: "/datos", ReadOnly: true}}
	casos := []struct {
		nombre string
		grafo  map[string]api.GraphNode
		nodos  []nodoMaquina
		nodo   string // "" = se acepta; si no, el nodo que nombra el rechazo
	}{
		{"sin volúmenes", map[string]api.GraphNode{"web": {}, "db": {}},
			[]nodoMaquina{{nodo: "web"}, {nodo: "db"}}, ""},
		{"solo lectura", map[string]api.GraphNode{"web": {Volumes: ro}},
			[]nodoMaquina{{nodo: "web", volumenes: ro}}, ""},
		{"escritura en la máquina", map[string]api.GraphNode{"web": {}, "db": {}},
			[]nodoMaquina{{nodo: "web"}, {nodo: "db", volumenes: rw}}, "db"},
		{"escritura en un lazy sin máquina", map[string]api.GraphNode{"web": {}, "worker": {Volumes: rw}},
			[]nodoMaquina{{nodo: "web"}}, "worker"},
		{"uno de varios en escritura", map[string]api.GraphNode{"web": {}},
			[]nodoMaquina{{nodo: "web", volumenes: append(append([]api.VolumeAttachment{}, ro...),
				api.VolumeAttachment{Name: "cache", Mount: "/cache"})}}, "web"},
		{"grafo vacío", map[string]api.GraphNode{}, nil, ""},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			err := volumenesDeFork(&api.Graph{Name: "g", Nodes: c.grafo}, c.nodos)
			if c.nodo == "" {
				if err != nil {
					t.Fatalf("rechazado: %v", err)
				}
				return
			}
			var se *api.StatusError
			if !errors.As(err, &se) || se.Code != 409 {
				t.Fatalf("err = %v, quiero un 409", err)
			}
			if !strings.Contains(err.Error(), "node "+c.nodo+" ") || !strings.Contains(err.Error(), ":ro") {
				t.Errorf("el rechazo no dice qué nodo ni cómo arreglarlo: %v", err)
			}
		})
	}
}

// credencialesDeFork solo deja pasar las credenciales que son aristas del
// grafo: de este grafo y con máquina destino.
func TestCredencialesDeFork(t *testing.T) {
	e := nuevaEscenaGrafo(t)
	g := e.montarGrafo(grafoTienda(true))
	web, apiID := e.maquina(g.ID, "web"), e.maquina(g.ID, "api")
	arista := credproxy.Credential{Env: "PGPASSWORD", Domain: "db.graph", Placeholder: "kling-cred-x", Secret: "s-clave-de-prueba",
		Kind: credproxy.KindPostgres, UpstreamMachine: idVirtual(g.ID, "db"), UpstreamOwner: g.ID}
	nodos := []nodoMaquina{{nodo: "web", id: web}, {nodo: "api", id: apiID}}

	if err := e.m.guardarCredenciales(apiID, []credproxy.Credential{arista}); err != nil {
		t.Fatal(err)
	}
	got, err := e.m.credencialesDeFork(g.ID, nodos)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["web"]) != 0 || len(got["api"]) != 1 || got["api"][0].Placeholder != "kling-cred-x" {
		t.Fatalf("credenciales por nodo: %+v", got)
	}

	for nombre, mal := range map[string]func(c *credproxy.Credential){
		"de otro grafo":      func(c *credproxy.Credential) { c.UpstreamOwner = "otro-grafo" },
		"sin máquina":        func(c *credproxy.Credential) { c.UpstreamMachine = "" },
		"suelta (sin dueño)": func(c *credproxy.Credential) { c.UpstreamOwner, c.UpstreamMachine = "", "" },
	} {
		t.Run(nombre, func(t *testing.T) {
			c := arista
			mal(&c)
			if err := e.m.guardarCredenciales(web, []credproxy.Credential{arista, c}); err != nil {
				t.Fatal(err)
			}
			var se *api.StatusError
			if _, err := e.m.credencialesDeFork(g.ID, nodos); !errors.As(err, &se) || se.Code != 409 || !strings.Contains(err.Error(), "node web") {
				t.Fatalf("err = %v, quiero un 409 que nombre a web", err)
			}
		})
	}

	// Un almacén que no se puede abrir es un error del nodo, no «sin credenciales».
	if err := os.WriteFile(e.m.credPath(web), []byte("no es un almacén sellado"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.credencialesDeFork(g.ID, nodos); err == nil || !strings.Contains(err.Error(), "node web") {
		t.Fatalf("err = %v, quiero el fallo al leer las de web", err)
	}
}
