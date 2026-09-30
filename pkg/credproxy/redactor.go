package credproxy

// Sustitución del marcador en la petición y redacción de la clave en la
// respuesta. Ver la documentación del paquete para qué se cubre y qué no.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
)

// sustituirQuery devuelve la query cruda q con el marcador cambiado por la
// clave (APIs que la piden como ?key=). La clave va percent-encoded, que es
// como iría si el SDK la hubiera puesto él.
func sustituirQuery(q string, cs []Credential) string {
	for _, c := range cs {
		if !c.Query {
			continue
		}
		q = strings.ReplaceAll(q, c.Placeholder, url.QueryEscape(c.Secret))
	}
	return q
}

// sustituir cambia el marcador por la clave en una cabecera. Authorization:
// Basic lleva usuario:clave en base64, así que el marcador no aparece en claro:
// se decodifica, se sustituye y se vuelve a codificar; devuelve además el par
// (base64 con clave, base64 con marcador) para que el redactor lo reconozca
// aunque el eco venga sin el prefijo "Basic ".
func sustituir(k, v string, cs []Credential) (string, [][2]string) {
	return sustituirMarcando(k, v, cs, nil)
}

// sustituirMarcando es sustituir anotando en usadas (si no es nil) qué
// credenciales se cambiaron de verdad: lo que el registro de auditoría llama
// creds.
func sustituirMarcando(k, v string, cs []Credential, usadas marcas) (string, [][2]string) {
	if strings.EqualFold(k, "Authorization") && len(v) > 6 && strings.EqualFold(v[:6], "basic ") {
		enc := strings.TrimSpace(v[6:])
		if raw, err := base64.StdEncoding.DecodeString(enc); err == nil {
			cambiado := false
			for i, c := range cs {
				if c.enCabecera(k) && bytes.Contains(raw, []byte(c.Placeholder)) {
					raw = bytes.ReplaceAll(raw, []byte(c.Placeholder), []byte(c.Secret))
					cambiado = true
					usadas.marcar(i)
				}
			}
			if cambiado {
				nuevo := base64.StdEncoding.EncodeToString(raw)
				return "Basic " + nuevo, [][2]string{{nuevo, enc}}
			}
		}
	}
	for i, c := range cs {
		if c.enCabecera(k) && strings.Contains(v, c.Placeholder) {
			v = strings.ReplaceAll(v, c.Placeholder, c.Secret)
			usadas.marcar(i)
		}
	}
	return v, nil
}

// marcas dice, por índice de credencial, cuáles se sustituyeron en una
// petición. Atómicas: el cuerpo de una subida chunked lo lee el transporte en
// su propia goroutine mientras el handler ya espera la respuesta. Un valor nil
// no anota nada.
type marcas []atomic.Bool

func (m marcas) marcar(i int) {
	if m != nil {
		m[i].Store(true)
	}
}

// de devuelve la función que marca la credencial i, o nil si no se anota.
func (m marcas) de(i int) func() {
	if m == nil {
		return nil
	}
	return func() { m[i].Store(true) }
}

// variantes son las formas en que un proveedor puede devolver una cadena en
// eco además de tal cual: escapada como JSON (\/ de PHP, \u003c de Go),
// percent-encoded (query, ruta y userinfo), como entidades HTML, en mayúsculas
// o minúsculas, en hex y en base64 (std y url, con y sin padding, y también el
// trozo central de la clave codificada en medio de otros datos, con cualquiera
// de los tres desfases posibles). Solo las que cambian algo.
//
// Es defensa en profundidad y NO cubre toda transformación: un proveedor que
// refleje lo que recibe (un LLM) puede devolverla con espacios, al revés o
// cifrada con César. Por eso el marcador solo se cambia por defecto en
// cabeceras (ver Credential.Body).
func variantes(s string) []string {
	out := []string{s}
	add := func(v string) {
		// Una forma derivada muy corta casaría con cualquier cosa (y rutaAuditada
		// enmascararía rutas enteras); la clave tal cual va siempre.
		if len(v) < minVariante {
			return
		}
		for _, o := range out {
			if o == v {
				return
			}
		}
		out = append(out, v)
	}
	if j, err := json.Marshal(s); err == nil && len(j) >= 2 {
		add(string(j[1 : len(j)-1]))
	}
	add(strings.ReplaceAll(s, "/", `\/`))
	add(url.QueryEscape(s))
	add(url.PathEscape(s))
	add(url.User(s).String())
	add(html.EscapeString(s))
	add(strings.ToUpper(s))
	add(strings.ToLower(s))
	b := []byte(s)
	add(hex.EncodeToString(b))
	add(strings.ToUpper(hex.EncodeToString(b)))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		add(enc.EncodeToString(b))
		sin := enc.WithPadding(base64.NoPadding)
		add(sin.EncodeToString(b))
		for k := range 3 {
			add(nucleoBase64(sin, b, k))
		}
	}
	return out
}

// minVariante: largo mínimo de una forma derivada de la clave.
const minVariante = 6

// nucleoBase64 es lo que sale SIEMPRE igual al codificar b en base64 cuando
// va precedido de k bytes cualesquiera (k = 0, 1 o 2) y seguido de otros:
// quita del principio los caracteres que mezclan bits de lo anterior y del
// final el grupo incompleto que mezclaría bits de lo siguiente.
func nucleoBase64(enc *base64.Encoding, b []byte, k int) string {
	buf := append(make([]byte, k), b...)
	n := len(buf) / 3 * 3
	e := enc.EncodeToString(buf[:n])
	salto := [3]int{0, 2, 3}[k]
	if len(e) <= salto {
		return ""
	}
	return e[salto:]
}

// redactor sustituye cada clave (y sus variantes) por su marcador en un flujo,
// aunque llegue partida entre dos escrituras. Retiene del final de cada trozo
// solo lo que PUEDE ser el comienzo de una clave partida, no un tamaño fijo:
// así un flujo de eventos (SSE) sale entero en cada escritura en vez de con la
// cola de un evento esperando al siguiente. Close/ReadFrom vacían lo retenido.
type redactor struct {
	w     io.Writer
	pares []par
	tail  []byte
}

// par es una sustitución old→new. marca, si no es nil, se llama cuando old
// aparece (el sustituidor la usa para saber qué credencial se usó).
type par struct {
	old, new []byte
	marca    func()
}

func nuevoRedactor(w io.Writer, cs []Credential) *redactor {
	r := &redactor{w: w}
	for _, c := range cs {
		for _, v := range variantes(c.Secret) {
			r.par(v, c.Placeholder)
		}
	}
	return r
}

// par añade una sustitución old→new. Una vacía o idéntica no aporta nada.
func (r *redactor) par(old, new string) {
	if old == "" || old == new {
		return
	}
	for _, p := range r.pares {
		if string(p.old) == old {
			return
		}
	}
	// De más larga a más corta: una forma que contiene a otra (el base64 de
	// un Basic entero contiene el trozo central del base64 de la clave) se
	// cambia entera antes de que la corta la rompa.
	i := len(r.pares)
	for i > 0 && len(r.pares[i-1].old) < len(old) {
		i--
	}
	r.pares = slices.Insert(r.pares, i, par{old: []byte(old), new: []byte(new)})
}

// texto redacta una cadena suelta (cabeceras, mensajes de error).
func (r *redactor) texto(s string) string {
	b := []byte(s)
	for _, p := range r.pares {
		b = bytes.ReplaceAll(b, p.old, p.new)
	}
	return string(b)
}

// retener dice cuántos bytes del final de buf hay que guardar porque coinciden
// con el comienzo de alguna clave: lo que sigue podría completarla.
func (r *redactor) retener(buf []byte) int {
	keep := 0
	for _, p := range r.pares {
		lim := min(len(p.old)-1, len(buf))
		for k := lim; k > keep; k-- {
			if buf[len(buf)-k] == p.old[0] && bytes.HasPrefix(p.old, buf[len(buf)-k:]) {
				keep = k
				break
			}
		}
	}
	return keep
}

func (r *redactor) Write(p []byte) (int, error) {
	buf := append(r.tail, p...)
	for _, pr := range r.pares {
		if pr.marca != nil && bytes.Contains(buf, pr.old) {
			pr.marca()
		}
		buf = bytes.ReplaceAll(buf, pr.old, pr.new)
	}
	keep := r.retener(buf)
	cut := len(buf) - keep
	if cut > 0 {
		if _, err := r.w.Write(buf[:cut]); err != nil {
			return 0, err
		}
		if f, ok := r.w.(http.Flusher); ok {
			f.Flush()
		}
	}
	r.tail = append([]byte(nil), buf[cut:]...)
	return len(p), nil
}

func (r *redactor) ReadFrom(src io.Reader) (int64, error) {
	var n int64
	b := make([]byte, 32<<10)
	for {
		k, err := src.Read(b)
		if k > 0 {
			if _, werr := r.Write(b[:k]); werr != nil {
				return n, werr
			}
			n += int64(k)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
	}
	return n, r.vaciar()
}

// vaciar escribe lo retenido: el flujo terminó y ya nada puede completarlo.
func (r *redactor) vaciar() error {
	if len(r.tail) == 0 {
		return nil
	}
	_, err := r.w.Write(r.tail)
	r.tail = nil
	return err
}

// sustChunk es cuánto lee el sustituidor del invitado de una vez. Pequeño a
// propósito: lo que acota la memoria no es el trozo leído sino lo que ocupa
// tras sustituir, y un trozo lleno de marcadores (47 bytes) que se cambian por
// claves de hasta MaxSecret crece ~87 veces. Con 4 KiB el peor caso son unos
// 350 KiB por petición.
const sustChunk = 4 << 10

// sustituidor cambia el marcador por la clave en el cuerpo de la PETICIÓN
// según pasa, sin retenerlo entero: es el redactor de la respuesta en sentido
// inverso (marcador→clave), con la misma ventana, así que un marcador partido
// entre dos lecturas también se cambia. Solo cambia el marcador tal cual: es
// [a-z0-9-] y ningún codificador habitual lo transforma.
type sustituidor struct {
	src   io.Reader
	red   *redactor
	out   bytes.Buffer
	chunk []byte
	err   error
}

func nuevoSustituidor(src io.Reader, cs []Credential) *sustituidor {
	return nuevoSustituidorMarcando(src, cs, nil)
}

// nuevoSustituidorMarcando es nuevoSustituidor anotando en usadas qué
// credenciales aparecieron en el cuerpo. Solo cambia las credenciales con Body:
// las demás pasan el marcador tal cual (ver Credential.Body). Los marcadores son distintos entre sí
// (ValidarCredenciales) y nunca iguales a su clave, así que cada credencial es
// exactamente un par.
func nuevoSustituidorMarcando(src io.Reader, cs []Credential, usadas marcas) *sustituidor {
	s := &sustituidor{src: src, chunk: make([]byte, sustChunk)}
	s.red = &redactor{w: &s.out}
	for i, c := range cs {
		if !c.Body {
			continue
		}
		s.red.pares = append(s.red.pares, par{old: []byte(c.Placeholder), new: []byte(c.Secret), marca: usadas.de(i)})
	}
	return s
}

func (s *sustituidor) Read(p []byte) (int, error) {
	for s.out.Len() == 0 {
		if s.err != nil {
			return 0, s.err
		}
		n, err := s.src.Read(s.chunk)
		if n > 0 {
			_, _ = s.red.Write(s.chunk[:n]) // escribe en un bytes.Buffer: no falla
		}
		if err != nil {
			if err == io.EOF {
				_ = s.red.vaciar()
			}
			s.err = err
		}
	}
	return s.out.Read(p)
}
