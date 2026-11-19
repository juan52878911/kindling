package telemetria

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Serie es una muestra del texto Prometheus ya clasificada.
type Serie struct {
	Nombre    string
	Tipo      string // gauge | counter | summary | untyped
	Etiquetas []Atributo
	Valor     float64
}

// ParsearPrometheus lee el formato de texto de Prometheus (lo que escribe
// frontal/metrics.go): # TYPE, muestras con etiquetas entre llaves y valor.
// En un summary, _sum y _count se tratan como contadores y las muestras con
// quantile como gauges con esa etiqueta.
func ParsearPrometheus(txt string) []Serie {
	tipos := map[string]string{}
	var out []Serie
	for _, l := range strings.Split(txt, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "#") {
			if f := strings.Fields(l); len(f) == 4 && f[1] == "TYPE" {
				tipos[f[2]] = f[3]
			}
			continue
		}
		nombre, resto := l, ""
		var et []Atributo
		if i := strings.IndexByte(l, '{'); i >= 0 {
			j := strings.LastIndexByte(l, '}')
			if j < i {
				continue
			}
			nombre, resto = l[:i], strings.TrimSpace(l[j+1:])
			et = parsearEtiquetas(l[i+1 : j])
		} else if f := strings.Fields(l); len(f) >= 2 {
			nombre, resto = f[0], f[1]
		} else {
			continue
		}
		f := strings.Fields(resto)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		tipo := tipos[nombre]
		if tipo == "" {
			for _, suf := range []string{"_sum", "_count"} {
				if b := strings.TrimSuffix(nombre, suf); b != nombre && tipos[b] == "summary" {
					tipo = "counter"
				}
			}
		}
		if tipo == "summary" {
			tipo = "gauge" // las muestras con quantile
		}
		if tipo == "" {
			tipo = "gauge"
		}
		out = append(out, Serie{Nombre: nombre, Tipo: tipo, Etiquetas: et, Valor: v})
	}
	return out
}

func parsearEtiquetas(s string) []Atributo {
	var out []Atributo
	for len(s) > 0 {
		eq := strings.Index(s, `="`)
		if eq < 0 {
			break
		}
		k := strings.TrimSpace(strings.TrimLeft(s[:eq], ", "))
		s = s[eq+2:]
		var v strings.Builder
		i := 0
		for ; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				if s[i] == 'n' {
					v.WriteByte('\n')
				} else {
					v.WriteByte(s[i])
				}
				continue
			}
			if s[i] == '"' {
				break
			}
			v.WriteByte(s[i])
		}
		out = append(out, Texto(k, v.String()))
		if i >= len(s) {
			break
		}
		s = s[i+1:]
	}
	return out
}

type jPunto struct {
	Attributes        []jAttr `json:"attributes"`
	StartTimeUnixNano string  `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string  `json:"timeUnixNano"`
	AsDouble          float64 `json:"asDouble"`
}

func (e *Exportador) bucleMetricas() {
	defer e.hecho.Done()
	t := time.NewTicker(e.o.IntervaloMetricas)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			e.ExportarMetricas(context.Background())
		case <-e.paro:
			return
		}
	}
}

// ExportarMetricas lee la fuente, añade las cuentas propias y las envía una vez.
func (e *Exportador) ExportarMetricas(ctx context.Context) {
	if e.o.Metricas == nil {
		return
	}
	txt, err := e.o.Metricas(ctx)
	if err != nil {
		e.erroresMet.Add(1)
		return
	}
	series := ParsearPrometheus(txt)
	series = append(series,
		Serie{Nombre: "kling_sandbox_otlp_spans_exported_total", Tipo: "counter", Valor: float64(e.enviados.Load())},
		Serie{Nombre: "kling_sandbox_otlp_spans_dropped_total", Tipo: "counter", Valor: float64(e.descartados.Load())},
		Serie{Nombre: "kling_sandbox_otlp_export_errors_total", Tipo: "counter", Valor: float64(e.errores.Load() + e.erroresMet.Load())},
	)
	if err := e.post("/v1/metrics", e.cuerpoMetricas(series, time.Now())); err != nil {
		e.erroresMet.Add(1)
	}
}

func (e *Exportador) cuerpoMetricas(series []Serie, ahora time.Time) []byte {
	orden := []string{}
	porNombre := map[string][]Serie{}
	for _, s := range series {
		if _, ok := porNombre[s.Nombre]; !ok {
			orden = append(orden, s.Nombre)
		}
		porNombre[s.Nombre] = append(porNombre[s.Nombre], s)
	}
	var ms []any
	for _, n := range orden {
		ss := porNombre[n]
		pts := make([]jPunto, 0, len(ss))
		for _, s := range ss {
			p := jPunto{Attributes: attrs(s.Etiquetas), TimeUnixNano: nano(ahora), AsDouble: s.Valor}
			if ss[0].Tipo == "counter" {
				p.StartTimeUnixNano = nano(e.inicio)
			}
			pts = append(pts, p)
		}
		m := map[string]any{"name": n}
		if ss[0].Tipo == "counter" {
			m["sum"] = map[string]any{"dataPoints": pts, "aggregationTemporality": 2, "isMonotonic": true}
		} else {
			m["gauge"] = map[string]any{"dataPoints": pts}
		}
		ms = append(ms, m)
	}
	doc := map[string]any{"resourceMetrics": []any{map[string]any{
		"resource": e.recurso(),
		"scopeMetrics": []any{map[string]any{
			"scope": jAlcance{"kindling/sandbox/gateway"}, "metrics": ms,
		}},
	}}}
	b, _ := json.Marshal(doc)
	return b
}
