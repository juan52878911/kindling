package machine

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/juan52878911/kindling/pkg/esquema"
)

// Esquemas son las versiones de los ficheros de estado versionados (ver
// docs/actualizar.md §3.1): las que sabe leer un binario o las que hay en una
// raíz de datos. Es lo que `kling upgrade` compara antes de cambiar un binario
// por otro: un daemon que no entiende el state.json que encuentra no arranca,
// y descubrirlo después de pararlo es tarde.
type Esquemas struct {
	Estado       int `json:"state"`       // state.json
	Meta         int `json:"meta"`        // snapshots/*/meta.json
	Credenciales int `json:"credentials"` // almacenes de credenciales (cabecera KLCS)
}

// EsquemasSoportados son las versiones más nuevas que este binario sabe leer.
func EsquemasSoportados() Esquemas {
	return Esquemas{Estado: versionEstadoMax, Meta: metaSchema, Credenciales: versionAlmacen}
}

// EsquemasEnDisco recorre la raíz de datos y devuelve la versión más alta de
// cada fichero versionado que encuentra. Solo lee cabeceras: ni descifra ni
// interpreta nada. Un fichero ilegible no cuenta (lo trata su dueño al
// cargarlo); uno que no se puede abrir, sí es un error, porque decidir sin
// verlo sería adivinar.
func EsquemasEnDisco(root string) (Esquemas, error) {
	var e Esquemas
	if b, err := os.ReadFile(filepath.Join(root, "state.json")); err == nil {
		if v, err := esquema.Version(b); err == nil {
			e.Estado = v
		}
	} else if !os.IsNotExist(err) {
		return e, err
	}
	metas, _ := filepath.Glob(filepath.Join(root, "snapshots", "*", "meta.json"))
	for _, p := range metas {
		b, err := os.ReadFile(p)
		if err != nil {
			return e, err
		}
		if v, err := esquema.Version(b); err == nil && v > e.Meta {
			e.Meta = v
		}
	}
	var almacenes []string
	for _, patron := range []string{
		filepath.Join(root, "machines", "*", credFile),
		filepath.Join(root, "secrets", "credentials", "*.enc"),
		filepath.Join(root, "store", "graph", "*.secrets.enc"),
	} {
		m, _ := filepath.Glob(patron)
		almacenes = append(almacenes, m...)
	}
	for _, p := range almacenes {
		v, err := versionDeAlmacen(p)
		if err != nil {
			return e, err
		}
		if v > e.Credenciales {
			e.Credenciales = v
		}
	}
	return e, nil
}

// versionDeAlmacen lee solo la cabecera de un almacén sellado.
func versionDeAlmacen(p string) (int, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cab := make([]byte, len(magiaAlmacen)+1)
	n, _ := f.Read(cab)
	return versionSellada(cab[:n]), nil
}

// Incompatibles dice qué de lo que hay en disco (disco) no podría leer un
// binario que soporta s, con un mensaje por fichero. Vacío = puede con todo.
func (s Esquemas) Incompatibles(disco Esquemas) []string {
	var out []string
	if disco.Estado > s.Estado {
		out = append(out, fmt.Sprintf("state.json has schema %d, the target reads up to %d: it would not start", disco.Estado, s.Estado))
	}
	if disco.Credenciales > s.Credenciales {
		out = append(out, fmt.Sprintf("credential stores have version %d, the target reads up to %d: it could not decrypt them", disco.Credenciales, s.Credenciales))
	}
	if disco.Meta > s.Meta {
		out = append(out, fmt.Sprintf("golden meta.json files have schema %d, the target reads up to %d: it would not restore them", disco.Meta, s.Meta))
	}
	return out
}

// Migraciones dice qué ficheros migrará un daemon que escribe s al arrancar
// sobre disco, con la copia que deja de cada uno.
func (s Esquemas) Migraciones(disco Esquemas) []string {
	var out []string
	// De state.json solo se migra el 0: el 2 no es una migración, lo escribe
	// quien tiene copias en diferencial (ver versionEstadoDiff).
	if disco.Estado == 0 && s.Estado > 0 {
		out = append(out, "state.json: schema 0 -> 1 on start (copy at state.json.v0.bak)")
	}
	if disco.Meta < s.Meta {
		out = append(out, fmt.Sprintf("golden meta.json: schema %d -> %d on their next write (copy at meta.json.v%d.bak)", disco.Meta, s.Meta, disco.Meta))
	}
	if disco.Credenciales < s.Credenciales {
		out = append(out, fmt.Sprintf("credential stores: version %d -> %d on their next sealing (copy at <file>.v%d.bak)", disco.Credenciales, s.Credenciales, disco.Credenciales))
	}
	return out
}

// ObsoletosEnDisco lista, con un mensaje por fichero, lo que hay en root de
// una época cuyas migraciones ya no están (docs/actualizar.md §5, PR 11): un
// state.json de v0.13 ("warm"), meta.json de v0.4 con los campos de MCP y un
// links.json de v0.4 sin migrar. Son los mismos mensajes con los que el daemon
// se niega a arrancar o a listar el dorado; `kling upgrade` los da antes de
// parar nada. Como EsquemasEnDisco, un fichero que no se puede abrir es un
// error y uno ilegible no cuenta.
func ObsoletosEnDisco(root string) ([]string, error) {
	var out []string
	if err := comprobarVersionEstado(root); err != nil && !esquema.EsMasNuevo(err) {
		out = append(out, err.Error())
	}
	metas, _ := filepath.Glob(filepath.Join(root, "snapshots", "*", "meta.json"))
	for _, p := range metas {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if v, err := esquema.Version(b); err == nil && v == 0 {
			if err := comprobarMetaV04(p, b); err != nil {
				out = append(out, err.Error())
			}
		}
	}
	if err := LinksV04(root); err != nil {
		out = append(out, err.Error())
	}
	return out, nil
}

// LinksV04 da error si en root hay un links.json de v0.4 que nadie migró al
// store (root/store/mcp/links.json). Hasta v0.4 los servidores MCP externos
// enlazados vivían ahí; desde v0.5 son de kindling-mcp, en ese documento, y
// hasta v0.17 el daemon los movía al arrancar. Ya no: el daemon se niega a
// arrancar diciendo cómo pasarla por v0.17, en vez de seguir sin esos enlaces.
// Si el store ya los tiene, el fichero es un resto (v0.17 lo dejaba si no podía
// renombrarlo) y no cuenta.
func LinksV04(root string) error {
	viejo := filepath.Join(root, "links.json")
	if _, err := os.Stat(viejo); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, "store", "mcp", "links.json")); err == nil {
		return nil
	}
	return fmt.Errorf("%s holds MCP links from kling v0.4, which this kling no longer migrates: "+
		"start kling v0.17 once on %s (it moves them to store/mcp/links), or remove the file if you do not need them", viejo, root)
}
