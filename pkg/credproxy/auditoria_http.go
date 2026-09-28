package credproxy

// La parte HTTP del registro de auditoría: qué se mide de cada petición y
// cómo se deja la ruta y el host sin nada que no deba ir a disco.

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// maxRutaAuditada, maxHostAuditado y maxMetodoAuditado acotan lo que el
	// invitado controla y acaba en el registro: una ruta de 60 KiB no debe
	// hacer de cada línea un bloque.
	maxRutaAuditada   = 256
	maxHostAuditado   = 253
	maxMetodoAuditado = 32
	// minTokenAuditado: un segmento de ruta de al menos esta longitud hecho
	// solo de caracteres de base64url (hex incluido) se escribe como ":tok".
	// Es la forma de un identificador opaco o una clave, y no aporta nada al
	// registro que no se pueda deducir de la ruta que lo rodea.
	minTokenAuditado = 32
)

// auditar deja el registro de una petición terminada (bien o con un rechazo).
func (p *Proxy) auditar(rec *Record, w *medidor, inicio time.Time, leidos *atomic.Int64, cs []Credential, usadas marcas) {
	if p.aud == nil {
		return
	}
	rec.TS = inicio
	rec.Status = w.estado()
	rec.ReqBytes = leidos.Load()
	rec.RespBytes = w.n
	rec.MS = time.Since(inicio).Milliseconds()
	for i := range usadas {
		if usadas[i].Load() {
			rec.Creds = append(rec.Creds, cs[i].Env)
		}
	}
	p.aud.Record(*rec)
}

// medidor envuelve el ResponseWriter para saber qué estado y cuántos bytes
// recibió el invitado. Unwrap es obligatorio: http.ResponseController (los
// plazos de plazosInvitado y el Flush de cada evento SSE) llega por él al
// ResponseWriter del servidor.
type medidor struct {
	w      http.ResponseWriter
	codigo int
	n      int64
}

func (m *medidor) Header() http.Header         { return m.w.Header() }
func (m *medidor) Unwrap() http.ResponseWriter { return m.w }

func (m *medidor) WriteHeader(code int) {
	if m.codigo == 0 && code >= 200 {
		m.codigo = code
	}
	m.w.WriteHeader(code)
}

func (m *medidor) Write(b []byte) (int, error) {
	if m.codigo == 0 {
		m.codigo = http.StatusOK
	}
	n, err := m.w.Write(b)
	m.n += int64(n)
	return n, err
}

// estado es el código que se mandó; 200 si el handler no escribió nada (es
// lo que manda el servidor en ese caso).
func (m *medidor) estado() int {
	if m.codigo == 0 {
		return http.StatusOK
	}
	return m.codigo
}

// lectorContado cuenta los bytes del cuerpo del invitado. Atómico: en una
// subida chunked lo lee el transporte desde su goroutine.
type lectorContado struct {
	io.ReadCloser
	n *atomic.Int64
}

func (l lectorContado) Read(b []byte) (int, error) {
	n, err := l.ReadCloser.Read(b)
	l.n.Add(int64(n))
	return n, err
}

// recortar acota s a max bytes.
func recortar(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// hostAuditado es el Host tal y como se buscó la credencial, acotado. Un Host
// que lleve un marcador (el invitado puede mandar lo que quiera) se escribe
// como ":cred".
func hostAuditado(host string) string {
	if contieneInsensible(host, PlaceholderPrefix) {
		return ":cred"
	}
	return recortar(host, maxHostAuditado)
}

// rutaAuditada es la ruta que va al registro: la normalizada (la que se
// compara con Allow), con cada segmento que lleve un marcador o una forma de
// alguna clave (ocultar, ver variantes) cambiado por ":cred", cada segmento
// con pinta de token opaco por ":tok", y acotada a maxRutaAuditada. La query
// no se mira: el registro solo dice si la había.
func rutaAuditada(u *url.URL, ocultar []string) string {
	ruta := rutaNormalizada(u)
	// Primero en la ruta entera, por si una clave lleva "/": partida en
	// segmentos ya no se reconocería. \x00 marca dónde estaba.
	for _, o := range ocultar {
		if o != "" {
			ruta = strings.ReplaceAll(ruta, o, "\x00")
		}
	}
	segs := strings.Split(ruta, "/")
	for i, s := range segs {
		switch {
		case strings.Contains(s, "\x00") || contieneInsensible(s, PlaceholderPrefix):
			segs[i] = ":cred"
		case pareceToken(s):
			segs[i] = ":tok"
		}
	}
	return recortar(strings.Join(segs, "/"), maxRutaAuditada)
}

// pareceToken: al menos minTokenAuditado caracteres, todos de base64url (que
// incluye el hex), con el relleno "=".
func pareceToken(s string) bool {
	if len(s) < minTokenAuditado {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '=':
		default:
			return false
		}
	}
	return true
}
