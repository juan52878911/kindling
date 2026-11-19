package e2b

// Connect RPC mínimo y sin dependencias: solo JSON. Unario = POST con
// `application/json`; streaming de servidor = `application/connect+json`, con
// sobres de 5 bytes (flags + longitud big-endian) y un sobre final con el
// flag 0x02 que lleva el error (o `{}`).

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	flagComprimido = 0x01
	flagFin        = 0x02

	tipoUnario    = "application/json"
	tipoStreaming = "application/connect+json"

	// maxSobre acota un sobre de petición: ninguna llamada admitida lleva más.
	maxSobre = 8 << 20
)

// Códigos de error de Connect y su HTTP, según la especificación del protocolo.
const (
	cNoEncontrado  = "not_found"
	cInvalido      = "invalid_argument"
	cNoImplementa  = "unimplemented"
	cInterno       = "internal"
	cNoDisponible  = "unavailable"
	cNoAutenticado = "unauthenticated"
	cPermiso       = "permission_denied"
	cCuota         = "resource_exhausted"
	cPlazo         = "deadline_exceeded"
	cAbortado      = "canceled"
	cYaExiste      = "already_exists"
)

func httpDeConnect(c string) int {
	switch c {
	case cNoEncontrado:
		return http.StatusNotFound
	case cInvalido:
		return http.StatusBadRequest
	case cYaExiste:
		return http.StatusConflict
	case cNoImplementa:
		return http.StatusNotImplemented
	case cNoAutenticado:
		return http.StatusUnauthorized
	case cPermiso:
		return http.StatusForbidden
	case cCuota:
		return http.StatusTooManyRequests
	case cNoDisponible:
		return http.StatusServiceUnavailable
	case cPlazo:
		return http.StatusGatewayTimeout
	case cAbortado:
		return 499
	}
	return http.StatusInternalServerError
}

// connectDeHTTP traduce el código HTTP del gateway al código Connect.
func connectDeHTTP(st int) string {
	switch {
	case st == 404:
		return cNoEncontrado
	case st == 400 || st == 413:
		return cInvalido
	case st == 401:
		return cNoAutenticado
	case st == 403:
		return cPermiso
	case st == 429:
		return cCuota
	case st == 501:
		return cNoImplementa
	case st == 504:
		return cPlazo
	case st == 502 || st == 503:
		return cNoDisponible
	}
	return cInterno
}

// errConnect es el cuerpo de error de Connect.
type errConnect struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *errConnect) Error() string { return e.Code + ": " + e.Message }

// escribirErrorUnario responde un error de una llamada unaria: HTTP según el
// código y cuerpo JSON {"code","message"}.
func escribirErrorUnario(w http.ResponseWriter, e *errConnect) {
	w.Header().Set("Content-Type", tipoUnario)
	w.WriteHeader(httpDeConnect(e.Code))
	json.NewEncoder(w).Encode(e)
}

// sobre construye un sobre Connect: flags, longitud y carga.
func sobre(flags byte, carga []byte) []byte {
	b := make([]byte, 5+len(carga))
	b[0] = flags
	binary.BigEndian.PutUint32(b[1:5], uint32(len(carga)))
	copy(b[5:], carga)
	return b
}

// leerSobre lee un sobre de petición. Rechaza el flag de compresión: el shim no
// anuncia ninguna codificación.
func leerSobre(r io.Reader) (flags byte, carga []byte, err error) {
	var cab [5]byte
	if _, err = io.ReadFull(r, cab[:]); err != nil {
		if err == io.EOF {
			return 0, nil, errors.New("empty request: the envelope is missing")
		}
		return 0, nil, fmt.Errorf("incomplete envelope header: %w", err)
	}
	n := binary.BigEndian.Uint32(cab[1:5])
	if n > maxSobre {
		return 0, nil, fmt.Errorf("envelope of %d bytes exceeds the %d byte limit", n, maxSobre)
	}
	carga = make([]byte, n)
	if _, err = io.ReadFull(r, carga); err != nil {
		return 0, nil, fmt.Errorf("incomplete envelope: %w", err)
	}
	if cab[0]&flagComprimido != 0 {
		return 0, nil, errors.New("compressed envelopes are not supported (send Connect-Content-Encoding: identity)")
	}
	return cab[0], carga, nil
}

// tipoContenido devuelve el tipo de contenido sin parámetros y en minúsculas.
func tipoContenido(r *http.Request) string {
	t := r.Header.Get("Content-Type")
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = t[:i]
	}
	return strings.ToLower(strings.TrimSpace(t))
}

// flujo escribe un stream de servidor Connect.
type flujo struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func nuevoFlujo(w http.ResponseWriter) *flujo {
	f := &flujo{w: w}
	f.fl, _ = w.(http.Flusher)
	w.Header().Set("Content-Type", tipoStreaming)
	w.Header().Set("Connect-Content-Encoding", "identity")
	return f
}

// mensaje escribe un mensaje de datos y lo vacía al momento.
func (f *flujo) mensaje(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err = f.w.Write(sobre(0, b)); err != nil {
		return err
	}
	if f.fl != nil {
		f.fl.Flush()
	}
	return nil
}

// fin escribe el sobre final: `{}` si todo fue bien, o {"error":{...}}.
func (f *flujo) fin(e *errConnect) {
	cuerpo := map[string]any{}
	if e != nil {
		cuerpo["error"] = e
	}
	b, _ := json.Marshal(cuerpo)
	f.w.Write(sobre(flagFin, b))
	if f.fl != nil {
		f.fl.Flush()
	}
}

// rechazarTipo responde a un cliente que pide un formato que el shim no habla
// (protobuf binario): 415 con un error Connect que dice qué hacer.
func rechazarTipo(w http.ResponseWriter, r *http.Request, quiero string) {
	w.Header().Set("Content-Type", tipoUnario)
	w.WriteHeader(http.StatusUnsupportedMediaType)
	json.NewEncoder(w).Encode(&errConnect{cNoImplementa,
		fmt.Sprintf("unsupported content type %q: this shim only speaks Connect with JSON (%s); configure the client for JSON instead of binary protobuf",
			r.Header.Get("Content-Type"), quiero)})
}
