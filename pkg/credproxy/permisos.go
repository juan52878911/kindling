package credproxy

// Permisos por método y ruta de una credencial (Credential.Allow).
//
// POR QUÉ: el proxy es un oráculo de la clave: el invitado no la lee, pero
// puede USARLA contra su dominio con cualquier método y ruta. Si el proveedor
// no deja emitir claves restringidas (o el operador no quiere depender de
// ello), Allow acota qué peticiones se firman: "GET /v1/balance" y nada más.
//
// FORMATO de una entrada: "MÉTODO /ruta". El método es exacto y de una lista
// fija (nada de comodines: HEAD no es GET). La ruta se compara por segmentos
// con path.Match: "*" casa un segmento entero o parte de él y no cruza "/";
// "**" solo puede ser el ÚLTIMO segmento y casa cero o más segmentos (así
// "/v1/files/**" cubre /v1/files y todo lo que cuelga de él). La query no
// cuenta. Una lista vacía es "todo" (lo de antes de que existiera Allow).
//
// NORMALIZACIÓN: la ruta de la petición se decodifica y se pasa por path.Clean
// antes de comparar, así "/v1/../admin" o "/v1/%2e%2e/admin" se comparan como
// "/admin". Y para que el proveedor no vea algo distinto de lo que se
// comprobó, cuando el dominio tiene alguna credencial con Allow el proxy
// reenvía esa ruta normalizada, no la que mandó el invitado (ver rutaSaliente).

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// MaxAllow: entradas de Allow por credencial.
const MaxAllow = 32

// maxEntradaAllow: longitud de una entrada; una ruta de API no se acerca.
const maxEntradaAllow = 256

// metodosAllow son los métodos que una entrada puede nombrar. CONNECT y TRACE
// quedan fuera: el proxy no hace túneles y TRACE devolvería en eco la
// petición con la clave dentro.
var metodosAllow = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// regla es una entrada de Allow ya partida.
type regla struct {
	metodo string
	segs   []string // segmentos del patrón, sin el "**" final
	resto  bool     // acababa en "**"
}

// ValidarPermiso comprueba una entrada de Allow y la devuelve normalizada
// (método en mayúsculas, un solo espacio).
func ValidarPermiso(e string) (string, error) {
	_, norm, err := parsearPermiso(e)
	return norm, err
}

// ValidarPermisos valida y NORMALIZA en el sitio una lista de Allow.
func ValidarPermisos(allow []string) error {
	if len(allow) > MaxAllow {
		return fmt.Errorf("at most %d allowed requests per credential", MaxAllow)
	}
	for i, e := range allow {
		norm, err := ValidarPermiso(e)
		if err != nil {
			return err
		}
		allow[i] = norm
	}
	return nil
}

func parsearPermiso(e string) (regla, string, error) {
	if len(e) > maxEntradaAllow {
		return regla{}, "", fmt.Errorf("allowed request %.40q...: longer than %d bytes", e, maxEntradaAllow)
	}
	campos := strings.Fields(e)
	if len(campos) != 2 {
		return regla{}, "", fmt.Errorf("allowed request %q must be \"METHOD /path\", e.g. \"GET /v1/balance\"", e)
	}
	m, p := strings.ToUpper(campos[0]), campos[1]
	if !slices.Contains(metodosAllow, m) {
		return regla{}, "", fmt.Errorf("allowed request %q: method must be one of %s", e, strings.Join(metodosAllow, ", "))
	}
	// El patrón tiene que estar ya limpio: uno como "/v1/../admin" o "/v1/" no
	// significa lo que parece, y mejor decirlo al guardar que casar otra cosa.
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return regla{}, "", fmt.Errorf("allowed request %q: the path must be absolute and clean (no //, . or .., no trailing /)", e)
	}
	r := regla{metodo: m}
	if p != "/" {
		r.segs = strings.Split(p[1:], "/")
	}
	for i, s := range r.segs {
		if s == "**" && i == len(r.segs)-1 {
			r.segs, r.resto = r.segs[:i], true
			break
		}
		if strings.Contains(s, "**") {
			return regla{}, "", fmt.Errorf("allowed request %q: ** is only valid as the whole last segment", e)
		}
		if _, err := path.Match(s, ""); err != nil {
			return regla{}, "", fmt.Errorf("allowed request %q: bad pattern in %q", e, s)
		}
	}
	return r, m + " " + p, nil
}

// casa dice si la regla permite method sobre la ruta ya normalizada (limpia y
// absoluta, ver rutaNormalizada).
func (r regla) casa(method, ruta string) bool {
	if method != r.metodo {
		return false
	}
	var segs []string
	if ruta != "/" {
		segs = strings.Split(ruta[1:], "/")
	}
	if len(segs) < len(r.segs) || (!r.resto && len(segs) != len(r.segs)) {
		return false
	}
	for i, pat := range r.segs {
		if ok, _ := path.Match(pat, segs[i]); !ok {
			return false
		}
	}
	return true
}

// compilarPermisos parte una lista de Allow ya validada.
func compilarPermisos(allow []string) ([]regla, error) {
	out := make([]regla, 0, len(allow))
	for _, e := range allow {
		r, _, err := parsearPermiso(e)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// rutaNormalizada es la ruta decodificada de u pasada por path.Clean: lo que
// se compara con Allow. Una ruta vacía es "/".
func rutaNormalizada(u *url.URL) string {
	p := u.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// urlSaliente es la URL que el proxy manda al proveedor cuando el dominio
// tiene permisos: la ruta NORMALIZADA (la que se comprobó), que al salir se
// escapa de nuevo, con la barra final si la llevaba (hay APIs que distinguen)
// y la query tal cual. Una %2F del invitado sale como "/": se comprobó como
// separador y así la ve también el proveedor.
func urlSaliente(u *url.URL) *url.URL {
	p := rutaNormalizada(u)
	if p != "/" && strings.HasSuffix(u.Path, "/") {
		p += "/"
	}
	return &url.URL{Path: p, RawQuery: u.RawQuery}
}
