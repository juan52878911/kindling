package telemetria

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// maxCuerpo es cuánto del cuerpo de una petición se guarda para la
	// auditoría; si no cabe, el evento lo marca (CuerpoCortado).
	maxCuerpo = 256 << 10
	// maxRespuesta es cuánto de la respuesta se guarda (solo el JSON del
	// sandbox creado).
	maxRespuesta = 16 << 10
)

// Identidad es lo que se sabe de quien llama.
type Identidad struct {
	Nombre string
	Admin  bool
}

// Evento es una petición terminada, con lo que el envoltorio pudo ver desde fuera.
type Evento struct {
	Inicio   time.Time
	Duracion time.Duration
	Metodo   string
	Ruta     Ruta
	Query    url.Values
	Tenant   string // "" si el token no abrió nada
	Admin    bool
	Estado   int
	Traza    Traza
	// Cuerpo es el principio del cuerpo de la petición (solo si Ruta.Cuerpo).
	Cuerpo        []byte
	CuerpoCortado bool
	// Respuesta es el principio de la respuesta (solo si Ruta.Respuesta).
	Respuesta []byte
	// BytesEntrada es lo que el handler leyó del cuerpo (tamaño de una subida).
	BytesEntrada int64
}

// Config del envoltorio.
type Config struct {
	// Identificar resuelve la cabecera Authorization; nil = nadie se identifica.
	Identificar func(authorization string) (Identidad, bool)
	// Exportador de spans; nil = no se exportan (la traza se sigue propagando).
	Exportador *Exportador
	// Observadores reciben cada evento al terminar (la auditoría).
	Observadores []func(*Evento)
	// Propios son rutas que el envoltorio atiende él mismo (GET /v1/audit):
	// si devuelve true, la petición no llega al handler de dentro.
	Propios func(w http.ResponseWriter, r *http.Request, id Identidad, ok bool) bool
}

// Medio envuelve next: abre un span por petición con el traceparent entrante,
// lo deja en el contexto (para propagarlo a los daemons), y al terminar
// exporta el span y avisa a los observadores. Nunca guarda el token.
func Medio(next http.Handler, c Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ini := time.Now()
		ruta := Clasificar(r.Method, r.URL.Path)
		tz := Nueva(r.Header.Get("Traceparent"))
		var id Identidad
		var ok bool
		if c.Identificar != nil {
			id, ok = c.Identificar(r.Header.Get("Authorization"))
		}
		ev := &Evento{Inicio: ini, Metodo: r.Method, Ruta: ruta, Query: r.URL.Query(), Traza: tz}
		if ok {
			ev.Tenant, ev.Admin = id.Nombre, id.Admin
		}
		if ruta.Cuerpo && r.Body != nil && r.Body != http.NoBody {
			b, _ := io.ReadAll(io.LimitReader(r.Body, maxCuerpo+1))
			if len(b) > maxCuerpo {
				ev.CuerpoCortado, b = true, b[:maxCuerpo]
			}
			ev.Cuerpo = b
			r.Body = restoCuerpo{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
		}
		var cuenta *contador
		if r.Body != nil && r.Body != http.NoBody && !ruta.Cuerpo {
			cuenta = &contador{ReadCloser: r.Body}
			r.Body = cuenta
		}
		rw := &escritor{ResponseWriter: w, guarda: ruta.Respuesta}
		w.Header().Set("Traceparent", tz.Traceparent())
		r = r.WithContext(ConTraza(r.Context(), tz))
		defer func() {
			ev.Duracion = time.Since(ini)
			ev.Estado = rw.estado()
			if ruta.Cuerpo {
				ev.BytesEntrada = int64(len(ev.Cuerpo))
			} else if cuenta != nil {
				ev.BytesEntrada = cuenta.n
			}
			ev.Respuesta = rw.buf.Bytes()
			if ruta.Plantilla != "/v1/health" {
				c.exportar(ev, r)
			}
			for _, o := range c.Observadores {
				o(ev)
			}
		}()
		if c.Propios != nil && c.Propios(rw, r, id, ok) {
			return
		}
		next.ServeHTTP(rw, r)
	})
}

func (c Config) exportar(ev *Evento, r *http.Request) {
	if c.Exportador == nil || !ev.Traza.Muestra {
		return
	}
	as := []Atributo{
		Texto("http.request.method", ev.Metodo),
		Texto("http.route", ev.Ruta.Plantilla),
		Entero("http.response.status_code", int64(ev.Estado)),
		Entero("kling.duration_ms", ev.Duracion.Milliseconds()),
	}
	if ev.Tenant != "" {
		as = append(as, Texto("kling.tenant", ev.Tenant))
	}
	if ev.Ruta.Sandbox != "" {
		as = append(as, Texto("kling.sandbox", ev.Ruta.Sandbox))
	}
	if ev.Ruta.Accion != "" {
		as = append(as, Texto("kling.action", ev.Ruta.Accion))
	}
	c.Exportador.Encolar(Span{
		Traza: ev.Traza, Nombre: ev.Metodo + " " + ev.Ruta.Plantilla,
		Inicio: ev.Inicio, Fin: ev.Inicio.Add(ev.Duracion),
		Error: ev.Estado >= 500, Atributos: as,
	})
}

type contador struct {
	io.ReadCloser
	n int64
}

func (c *contador) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

type restoCuerpo struct {
	io.Reader
	io.Closer
}

// escritor recuerda el código y, si se pide, el principio de la respuesta.
// Deja pasar Flush y Hijack (las shells secuestran la conexión) y se deja
// desenvolver (http.ResponseController).
type escritor struct {
	http.ResponseWriter
	cod    int
	guarda bool
	buf    bytes.Buffer
}

func (w *escritor) estado() int {
	if w.cod == 0 {
		return http.StatusOK
	}
	return w.cod
}

func (w *escritor) WriteHeader(c int) {
	if w.cod == 0 && (c >= 200 || c == http.StatusSwitchingProtocols) {
		w.cod = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *escritor) Write(p []byte) (int, error) {
	if w.cod == 0 {
		w.cod = http.StatusOK
	}
	if w.guarda && w.buf.Len() < maxRespuesta {
		w.buf.Write(p[:min(len(p), maxRespuesta-w.buf.Len())])
	}
	return w.ResponseWriter.Write(p)
}

func (w *escritor) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *escritor) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	c, rw, err := h.Hijack()
	if err == nil && w.cod == 0 {
		w.cod = http.StatusSwitchingProtocols
	}
	return c, rw, err
}

func (w *escritor) Unwrap() http.ResponseWriter { return w.ResponseWriter }
