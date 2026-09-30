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
// reenvía esa ruta normalizada, no la que mandó el invitado (ver urlSaliente).
//
// AMBIGÜEDAD: normalizar asume que "/", "." y ".." significan lo mismo para
// nosotros que para el proveedor, y eso no siempre es así (%2F, ';', barra
// invertida...). Por eso, cuando el dominio tiene alguna credencial con
// Allow, ServeHTTP rechaza con 403 —antes de mirar el método o la ruta contra
// las reglas— cualquier petición cuya ruta CRUDA sea ambigua en ese sentido
// (ver rutaAmbigua). Sin ninguna credencial con Allow esto no se comprueba: el
// comportamiento de antes de F1 no cambia.
//
// QUERY: no entra en la comparación ni se considera para la ambigüedad; una
// entrada de Allow no puede acotar parámetros de query. Si algún día hiciera
// falta, que sea una entrada explícita en el formato, no una regla implícita
// aquí.

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

// rutaAmbigua mira la ruta CRUDA de la petición (sin decodificar, tal y como
// llegó por el cable) y dice, con un motivo, si tiene algo que un proveedor
// podría leer de otro modo que como la compara Allow: path.Clean asume que
// "/" separa segmentos y que ".."/"." navegan, pero eso es UNA interpretación,
// no la única. Un %2F, un %5C o una barra invertida literal pueden ser, para
// el proveedor, un carácter normal de un segmento en vez de un separador; un
// ";algo" puede ser un parámetro de ruta que su framework quita antes de
// enrutar; y una "//" puede colapsar antes o después de mirar el método. Con
// Allow de por medio no vale la pena intentar adivinar esa interpretación:
// mejor rechazar la ambigüedad que arriesgarse a firmar algo que el proveedor
// acaba viendo como una ruta distinta de la que se comprobó.
//
// Sin ninguna credencial del dominio con Allow no se llama a esta función: el
// invitado puede mandar la ruta que quiera y sale tal cual, como antes de que
// existiera Allow (ver ServeHTTP).
func rutaAmbigua(u *url.URL) string {
	crudo := u.EscapedPath()
	switch {
	case strings.Contains(crudo, "\\"):
		return "backslash in the path"
	case strings.Contains(crudo, "//"):
		return "double slash in the path"
	case contieneInsensible(crudo, "%5c"):
		return "encoded backslash (%5C) in the path"
	case contieneInsensible(crudo, "%2f"):
		return "encoded slash (%2F) in the path"
	}
	p := strings.TrimPrefix(crudo, "/")
	for _, seg := range strings.Split(p, "/") {
		if strings.Contains(seg, ";") {
			return "path parameter (;) in the path"
		}
		if d := decodificarPuntos(seg); d == "." || d == ".." {
			return "dot segment in the path"
		}
	}
	return rutaDecodificadaAmbigua(u.Path)
}

// rutaDecodificadaAmbigua mira la ruta YA decodificada una vez (u.Path): lo
// crudo puede esconder con percent-encoding lo que rutaAmbigua busca, y al
// salir la ruta va decodificada y reescapada (urlSaliente), así que un %3B
// llega al proveedor como ";" (/public/..%3B/admin sale como /public/..;/admin,
// que Tomcat o Spring leen como /admin). Un "%" que quede tras decodificar es
// una doble codificación (%252e): un proveedor que decodifique otra vez vería
// otra ruta. Y un carácter de control (%00, %0A) no tiene lectura única.
func rutaDecodificadaAmbigua(dec string) string {
	switch {
	case strings.Contains(dec, ";"):
		return "encoded path parameter (%3B) in the path"
	case strings.Contains(dec, "%"):
		return "double encoding (%25) in the path"
	case strings.Contains(dec, "\\"):
		return "backslash in the decoded path"
	case strings.Contains(dec, "//"):
		return "double slash in the decoded path"
	case strings.IndexFunc(dec, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return "control character in the path"
	}
	for _, seg := range strings.Split(strings.TrimPrefix(dec, "/"), "/") {
		if seg == "." || seg == ".." {
			return "dot segment in the decoded path"
		}
	}
	return ""
}

// contieneInsensible dice si s contiene tok (en minúsculas) sin mirar
// mayúsculas: un %2F y un %2f son la misma ambigüedad.
func contieneInsensible(s, tokMinusculas string) bool {
	return strings.Contains(strings.ToLower(s), tokMinusculas)
}

// decodificarPuntos cambia cada %2e/%2E de seg por ".", para reconocer un
// segmento que codifica (del todo o en parte) "." o ".." aunque no llegue a
// decodificarse por el camino normal. No toca nada más: no es un decodificador
// de percent-encoding general, solo lo justo para esta comprobación.
func decodificarPuntos(seg string) string {
	var b strings.Builder
	for i := 0; i < len(seg); {
		if i+3 <= len(seg) && seg[i] == '%' && seg[i+1] == '2' && (seg[i+2] == 'e' || seg[i+2] == 'E') {
			b.WriteByte('.')
			i += 3
			continue
		}
		b.WriteByte(seg[i])
		i++
	}
	return b.String()
}
