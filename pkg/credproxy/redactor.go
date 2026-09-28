package credproxy

// Sustitución del marcador en la petición y redacción de la clave en la
// respuesta. Ver la documentación del paquete para qué se cubre y qué no.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// sustituirQuery devuelve path+query con el marcador cambiado por la clave en
// la query (APIs que la piden como ?key=). La clave va percent-encoded, que es
// como iría si el SDK la hubiera puesto él.
func sustituirQuery(u *url.URL, cs []Credential) string {
	uri := u.RequestURI()
	if u.RawQuery == "" {
		return uri
	}
	q := u.RawQuery
	for _, c := range cs {
		q = strings.ReplaceAll(q, c.Placeholder, url.QueryEscape(c.Secret))
	}
	if q == u.RawQuery {
		return uri
	}
	v := *u
	v.RawQuery = q
	return v.RequestURI()
}

// sustituir cambia el marcador por la clave en una cabecera. Authorization:
// Basic lleva usuario:clave en base64, así que el marcador no aparece en claro:
// se decodifica, se sustituye y se vuelve a codificar; devuelve además el par
// (base64 con clave, base64 con marcador) para que el redactor lo reconozca
// aunque el eco venga sin el prefijo "Basic ".
func sustituir(k, v string, cs []Credential) (string, [][2]string) {
	if strings.EqualFold(k, "Authorization") && len(v) > 6 && strings.EqualFold(v[:6], "basic ") {
		enc := strings.TrimSpace(v[6:])
		if raw, err := base64.StdEncoding.DecodeString(enc); err == nil {
			cambiado := false
			for _, c := range cs {
				if bytes.Contains(raw, []byte(c.Placeholder)) {
					raw = bytes.ReplaceAll(raw, []byte(c.Placeholder), []byte(c.Secret))
					cambiado = true
				}
			}
			if cambiado {
				nuevo := base64.StdEncoding.EncodeToString(raw)
				return "Basic " + nuevo, [][2]string{{nuevo, enc}}
			}
		}
	}
	for _, c := range cs {
		v = strings.ReplaceAll(v, c.Placeholder, c.Secret)
	}
	return v, nil
}

// variantes son las formas en que un proveedor suele devolver una cadena en
// eco además de tal cual: escapada como JSON (\/ de PHP, < de Go),
// percent-encoded y como entidades HTML. Solo las que cambian algo.
func variantes(s string) []string {
	out := []string{s}
	add := func(v string) {
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
	add(html.EscapeString(s))
	return out
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

type par struct{ old, new []byte }

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
	r.pares = append(r.pares, par{[]byte(old), []byte(new)})
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
	s := &sustituidor{src: src, chunk: make([]byte, sustChunk)}
	s.red = &redactor{w: &s.out}
	for _, c := range cs {
		s.red.par(c.Placeholder, c.Secret)
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
