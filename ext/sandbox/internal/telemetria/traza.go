// Package telemetria exporta trazas y métricas del gateway por OTLP/HTTP con
// JSON, sin el SDK de OpenTelemetry (esta extensión no tiene dependencias).
//
// Apagado por defecto: sin -otlp-endpoint no se instala nada y las peticiones
// no pasan por aquí. Encendido, una petición cuesta unas asignaciones y un
// envío a un canal que no bloquea: si el colector no responde, los spans se
// descartan y se cuentan, nunca esperan a nadie.
package telemetria

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// Traza es el contexto W3C (traceparent) de una petición.
type Traza struct {
	TraceID string // 32 hex
	SpanID  string // 16 hex
	Padre   string // span del que viene, vacío si somos raíz
	Muestra bool   // bandera sampled
}

// Traceparent da la cabecera W3C de esta traza.
func (t Traza) Traceparent() string {
	f := "00"
	if t.Muestra {
		f = "01"
	}
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + f
}

// ParsearTraceparent lee la cabecera W3C. Rechaza lo mal formado y los ids a cero.
func ParsearTraceparent(v string) (Traza, bool) {
	p := strings.Split(strings.TrimSpace(v), "-")
	if len(p) < 4 || p[0] != "00" && len(p[0]) != 2 || p[0] == "ff" {
		return Traza{}, false
	}
	if !hexN(p[1], 32) || !hexN(p[2], 16) || !hexN(p[3], 2) || allZero(p[1]) || allZero(p[2]) {
		return Traza{}, false
	}
	// La versión 00 tiene exactamente 4 campos.
	if p[0] == "00" && len(p) != 4 {
		return Traza{}, false
	}
	fl, _ := hex.DecodeString(p[3])
	return Traza{TraceID: p[1], SpanID: p[2], Muestra: fl[0]&1 == 1}, true
}

func hexN(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func allZero(s string) bool { return strings.Trim(s, "0") == "" }

func aleatorio(n int) string {
	b := make([]byte, n)
	for {
		_, _ = rand.Read(b)
		s := hex.EncodeToString(b)
		if !allZero(s) {
			return s
		}
	}
}

// Nueva abre un span hijo de la cabecera entrante (si es válida) o una traza
// nueva. Siempre devuelve una Traza usable.
func Nueva(traceparentEntrante string) Traza {
	if p, ok := ParsearTraceparent(traceparentEntrante); ok {
		return Traza{TraceID: p.TraceID, SpanID: aleatorio(8), Padre: p.SpanID, Muestra: p.Muestra}
	}
	return Traza{TraceID: aleatorio(16), SpanID: aleatorio(8), Muestra: true}
}

type claveTraza struct{}

// ConTraza guarda la traza en el contexto.
func ConTraza(ctx context.Context, t Traza) context.Context {
	return context.WithValue(ctx, claveTraza{}, t)
}

// DeContexto saca la traza del contexto.
func DeContexto(ctx context.Context) (Traza, bool) {
	t, ok := ctx.Value(claveTraza{}).(Traza)
	return t, ok
}

// Inyectar pone traceparent en h si el contexto lleva una traza.
func Inyectar(ctx context.Context, h http.Header) {
	if t, ok := DeContexto(ctx); ok {
		h.Set("Traceparent", t.Traceparent())
	}
}

// Transporte envuelve un RoundTripper para que cada petición saliente lleve el
// traceparent del contexto (hacia los daemons).
func Transporte(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return propagador{rt}
}

type propagador struct{ rt http.RoundTripper }

func (p propagador) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, ok := DeContexto(r.Context()); ok {
		r = r.Clone(r.Context())
		Inyectar(r.Context(), r.Header)
	}
	return p.rt.RoundTrip(r)
}
