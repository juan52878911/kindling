package aigw

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"time"
)

// RESPUESTAS DE UN BACKEND HACIA EL CLIENTE.
//
// Lo que contesta una réplica (o un upstream) no es de fiar: decide el código,
// el cuerpo y las cabeceras. El gateway copia el código y el cuerpo, acotado
// en bytes y en tiempo, y de las cabeceras solo un Content-Type de la lista
// blanca: con -no-auth en loopback, un invitado que sirviera HTML lo serviría
// bajo el origen del gateway.

// tiposPermitidos son los Content-Type que pasan del backend al cliente; el
// resto sale como application/octet-stream, que un navegador no interpreta.
var tiposPermitidos = map[string]bool{
	"application/json":     true,
	"text/event-stream":    true,
	"application/x-ndjson": true,
}

// tipoContenido devuelve el Content-Type que se le da al cliente: el tipo del
// backend sin parámetros si está en la lista blanca (JSON y SSE son UTF-8 por
// definición), application/json si no dijo nada y octet-stream si dijo otro.
func tipoContenido(ct string) string {
	if ct == "" {
		return "application/json"
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || !tiposPermitidos[mt] {
		return "application/octet-stream"
	}
	return mt
}

// esFlujo dice si una respuesta se manda a trozos según llegan: SSE y NDJSON.
func esFlujo(mt string) bool {
	return mt == "text/event-stream" || mt == "application/x-ndjson"
}

// claseCodigo agrupa un código HTTP por clase para las métricas: el código lo
// elige el backend y no puede abrir una serie nueva por cada valor.
func claseCodigo(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// devolver copia resp al cliente. name es el modelo (para métricas y log);
// stream fuerza el vaciado tras cada lectura aunque el tipo no sea de flujo
// (el proxy OpenAI lo sabe por "stream": true del cuerpo).
//
// Pasado MaxProxyBytes la respuesta se corta. Las cabeceras ya salieron, así
// que no cabe un error limpio: se aborta la conexión (abortar) para que el
// cliente vea un cuerpo incompleto como error y no un 200 con JSON a medias.
func (g *Gateway) devolver(w http.ResponseWriter, resp *http.Response, name string, stream bool) {
	code := resp.StatusCode
	if code < 200 || code > 599 {
		code = http.StatusBadGateway
	}
	g.met.inc(g.met.proxy, name, claseCodigo(code))
	mt := tipoContenido(resp.Header.Get("Content-Type"))
	stream = stream || esFlujo(mt)
	h := w.Header()
	h.Set("Content-Type", mt)
	h.Set("X-Content-Type-Options", "nosniff")
	if stream {
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(code)

	// Copia acotada. Con streaming se vacía el búfer tras cada lectura para
	// que cada token llegue cuando sale; sin él, al final.
	rc := http.NewResponseController(w)
	// Sin WriteTimeout en el servidor, el plazo se queda en la conexión:
	// quitarlo al acabar para la siguiente petición de la misma conexión.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if n+int64(k) > g.opts.MaxProxyBytes {
				log.Printf("proxy %s: answer over %d bytes, cut", name, g.opts.MaxProxyBytes)
				g.met.vonErr(name, "too_large")
				abortar(rc)
				return
			}
			n += int64(k)
			_ = rc.SetWriteDeadline(time.Now().Add(plazoEscritura))
			if _, werr := w.Write(buf[:k]); werr != nil {
				return // el cliente se fue; el contexto cancela la petición al backend
			}
			if stream {
				if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
					return
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				g.met.vonErr(name, "stream")
				abortar(rc) // también cortada: que no parezca completa
			}
			return
		}
	}
}

// abortar cierra la conexión del cliente sin terminar la respuesta: en
// HTTP/1.1 falta el último trozo (o sobran bytes del Content-Length) y el
// cliente lo lee como error. Si el escritor no deja secuestrar la conexión
// (un ResponseRecorder, HTTP/2), la respuesta acaba ahí sin más, como antes.
func abortar(rc *http.ResponseController) {
	conn, _, err := rc.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}
