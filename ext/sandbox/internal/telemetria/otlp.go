package telemetria

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Valores por defecto del exportador.
const (
	ColaPorDefecto     = 2048
	LotePorDefecto     = 256
	CadaPorDefecto     = 5 * time.Second
	PlazoPorDefecto    = 5 * time.Second
	MetricasPorDefecto = 30 * time.Second
)

// Opciones del exportador.
type Opciones struct {
	// Endpoint base del colector ("http://colector:4318"); se le añade
	// /v1/traces y /v1/metrics.
	Endpoint string
	// Cabeceras extra (autenticación del colector).
	Cabeceras map[string]string
	// Servicio es service.name; Version service.version.
	Servicio, Version string
	// Cola es cuántos spans caben esperando (tope de memoria); lo que no cabe se descarta.
	Cola int
	// Lote es el máximo de spans por envío; Cada, cada cuánto se envía lo que haya.
	Lote int
	Cada time.Duration
	// Plazo acota cada envío al colector.
	Plazo time.Duration
	// Metricas, si no es nil, se llama cada IntervaloMetricas y devuelve el
	// texto Prometheus a exportar.
	Metricas          func(context.Context) (string, error)
	IntervaloMetricas time.Duration
	// Cliente HTTP para el colector (tests); nil = uno propio.
	Cliente *http.Client
}

// Atributo es un par clave/valor de OTLP (valor de texto o entero).
type Atributo struct {
	Clave string
	Texto string
	Num   int64
	EsNum bool
}

// Texto y Entero construyen atributos.
func Texto(k, v string) Atributo        { return Atributo{Clave: k, Texto: v} }
func Entero(k string, v int64) Atributo { return Atributo{Clave: k, Num: v, EsNum: true} }

// Span es un tramo terminado.
type Span struct {
	Traza     Traza
	Nombre    string
	Inicio    time.Time
	Fin       time.Time
	Error     bool
	Atributos []Atributo
}

// Exportador envía spans y métricas por OTLP/HTTP+JSON.
type Exportador struct {
	o      Opciones
	cl     *http.Client
	cola   chan Span
	paro   chan struct{}
	hecho  sync.WaitGroup
	inicio time.Time
	cierra sync.Once

	enviados    atomic.Int64
	descartados atomic.Int64
	errores     atomic.Int64
	erroresMet  atomic.Int64
}

// Nuevo valida las opciones y arranca las goroutines. Hay que llamar a Cerrar.
func Nuevo(o Opciones) (*Exportador, error) {
	u, err := url.Parse(o.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("otlp endpoint %q: expected http(s)://host:port", o.Endpoint)
	}
	o.Endpoint = strings.TrimRight(o.Endpoint, "/")
	if o.Servicio == "" {
		o.Servicio = "kindling-sandbox-gateway"
	}
	if o.Cola <= 0 {
		o.Cola = ColaPorDefecto
	}
	if o.Lote <= 0 {
		o.Lote = LotePorDefecto
	}
	if o.Cada <= 0 {
		o.Cada = CadaPorDefecto
	}
	if o.Plazo <= 0 {
		o.Plazo = PlazoPorDefecto
	}
	if o.IntervaloMetricas <= 0 {
		o.IntervaloMetricas = MetricasPorDefecto
	}
	e := &Exportador{o: o, cl: o.Cliente, cola: make(chan Span, o.Cola), paro: make(chan struct{}), inicio: time.Now()}
	if e.cl == nil {
		e.cl = &http.Client{Timeout: o.Plazo}
	}
	e.hecho.Add(1)
	go e.bucleSpans()
	if o.Metricas != nil {
		e.hecho.Add(1)
		go e.bucleMetricas()
	}
	return e, nil
}

// Encolar deja el span para el próximo envío. No bloquea nunca: con la cola
// llena (colector caído o lento) el span se descarta y se cuenta.
func (e *Exportador) Encolar(s Span) {
	select {
	case e.cola <- s:
	default:
		e.descartados.Add(1)
	}
}

// Descartados es cuántos spans se han perdido (cola llena o envío fallido).
func (e *Exportador) Descartados() int64 { return e.descartados.Load() }

// Enviados es cuántos spans aceptó el colector.
func (e *Exportador) Enviados() int64 { return e.enviados.Load() }

// Cerrar para las goroutines e intenta enviar lo que quede, sin pasar de ctx.
func (e *Exportador) Cerrar(ctx context.Context) {
	e.cierra.Do(func() { close(e.paro) })
	fin := make(chan struct{})
	go func() { e.hecho.Wait(); close(fin) }()
	select {
	case <-fin:
	case <-ctx.Done():
	}
}

func (e *Exportador) bucleSpans() {
	defer e.hecho.Done()
	t := time.NewTicker(e.o.Cada)
	defer t.Stop()
	lote := make([]Span, 0, e.o.Lote)
	vaciar := func() {
		if len(lote) > 0 {
			e.enviarSpans(lote)
			lote = lote[:0]
		}
	}
	for {
		select {
		case s := <-e.cola:
			lote = append(lote, s)
			if len(lote) >= e.o.Lote {
				vaciar()
			}
		case <-t.C:
			vaciar()
		case <-e.paro:
			for {
				select {
				case s := <-e.cola:
					lote = append(lote, s)
					if len(lote) >= e.o.Lote {
						vaciar()
					}
				default:
					vaciar()
					return
				}
			}
		}
	}
}

func (e *Exportador) enviarSpans(lote []Span) {
	cuerpo := e.cuerpoSpans(lote)
	if err := e.post("/v1/traces", cuerpo); err != nil {
		e.errores.Add(1)
		e.descartados.Add(int64(len(lote)))
		return
	}
	e.enviados.Add(int64(len(lote)))
}

func (e *Exportador) post(ruta string, cuerpo []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), e.o.Plazo)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, e.o.Endpoint+ruta, bytes.NewReader(cuerpo))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range e.o.Cabeceras {
		r.Header.Set(k, v)
	}
	resp, err := e.cl.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collector answered %s", resp.Status)
	}
	return nil
}

// ---- JSON de OTLP

type jValor struct {
	S *string  `json:"stringValue,omitempty"`
	I *string  `json:"intValue,omitempty"`
	D *float64 `json:"doubleValue,omitempty"`
}
type jAttr struct {
	Key   string `json:"key"`
	Value jValor `json:"value"`
}

func attrs(as []Atributo) []jAttr {
	out := make([]jAttr, 0, len(as))
	for _, a := range as {
		v := jValor{}
		if a.EsNum {
			s := strconv.FormatInt(a.Num, 10)
			v.I = &s
		} else {
			s := a.Texto
			v.S = &s
		}
		out = append(out, jAttr{a.Clave, v})
	}
	return out
}

type jRecurso struct {
	Attributes []jAttr `json:"attributes"`
}
type jAlcance struct {
	Name string `json:"name"`
}

func (e *Exportador) recurso() jRecurso {
	as := []Atributo{Texto("service.name", e.o.Servicio)}
	if e.o.Version != "" {
		as = append(as, Texto("service.version", e.o.Version))
	}
	return jRecurso{attrs(as)}
}

func nano(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

type jSpan struct {
	TraceID           string  `json:"traceId"`
	SpanID            string  `json:"spanId"`
	ParentSpanID      string  `json:"parentSpanId,omitempty"`
	Name              string  `json:"name"`
	Kind              int     `json:"kind"`
	StartTimeUnixNano string  `json:"startTimeUnixNano"`
	EndTimeUnixNano   string  `json:"endTimeUnixNano"`
	Attributes        []jAttr `json:"attributes"`
	Status            struct {
		Code int `json:"code"`
	} `json:"status"`
}

func (e *Exportador) cuerpoSpans(lote []Span) []byte {
	ss := make([]jSpan, 0, len(lote))
	for _, s := range lote {
		j := jSpan{
			TraceID: s.Traza.TraceID, SpanID: s.Traza.SpanID, ParentSpanID: s.Traza.Padre,
			Name: s.Nombre, Kind: 2, // SERVER
			StartTimeUnixNano: nano(s.Inicio), EndTimeUnixNano: nano(s.Fin),
			Attributes: attrs(s.Atributos),
		}
		j.Status.Code = 1 // OK
		if s.Error {
			j.Status.Code = 2
		}
		ss = append(ss, j)
	}
	doc := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": e.recurso(),
		"scopeSpans": []any{map[string]any{
			"scope": jAlcance{"kindling/sandbox/gateway"}, "spans": ss,
		}},
	}}}
	b, _ := json.Marshal(doc)
	return b
}
