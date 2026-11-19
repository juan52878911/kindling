package telemetria

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// colector es un colector OTLP falso: guarda lo que recibe por ruta.
type colector struct {
	*httptest.Server
	mu   sync.Mutex
	trz  []map[string]any
	met  []map[string]any
	auth string
}

func nuevoColector(t *testing.T) *colector {
	c := &colector{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad", 400)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.auth = r.Header.Get("X-Test-Auth")
		switch r.URL.Path {
		case "/v1/traces":
			c.trz = append(c.trz, m)
		case "/v1/metrics":
			c.met = append(c.met, m)
		default:
			http.NotFound(w, r)
		}
		w.Write([]byte("{}"))
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *colector) spans() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, m := range c.trz {
		for _, rs := range m["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, s := range ss.(map[string]any)["spans"].([]any) {
					out = append(out, s.(map[string]any))
				}
			}
		}
	}
	return out
}

func attr(s map[string]any, k string) any {
	for _, a := range s["attributes"].([]any) {
		a := a.(map[string]any)
		if a["key"] == k {
			v := a["value"].(map[string]any)
			if x, ok := v["stringValue"]; ok {
				return x
			}
			return v["intValue"]
		}
	}
	return nil
}

func quedan(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting for condition")
}

const tokenSecreto = "tok-super-secreto-1234567890"

func identificar(a string) (Identidad, bool) {
	switch a {
	case "Bearer " + tokenSecreto:
		return Identidad{Nombre: "ana"}, true
	case "Bearer tok-admin-0987654321xx":
		return Identidad{Nombre: "root", Admin: true}, true
	}
	return Identidad{}, false
}

func TestSpanPorPeticionConAtributosYTraceparent(t *testing.T) {
	col := nuevoColector(t)
	exp, err := Nuevo(Opciones{Endpoint: col.URL, Cada: 20 * time.Millisecond, Cabeceras: map[string]string{"X-Test-Auth": "k"}})
	if err != nil {
		t.Fatal(err)
	}
	// Lo que vería un daemon: el traceparent saliente sale del contexto de la petición.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Traceparent", r.Header.Get("Traceparent"))
	}))
	defer daemon.Close()
	cl := &http.Client{Transport: Transporte(nil)}
	var visto Traza
	var salida string
	h := Medio(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visto, _ = DeContexto(r.Context())
		rq, _ := http.NewRequestWithContext(r.Context(), "GET", daemon.URL, nil)
		resp, err := cl.Do(rq)
		if err != nil {
			t.Error(err)
			return
		}
		salida = resp.Header.Get("X-Seen-Traceparent")
		resp.Body.Close()
		w.WriteHeader(201)
	}), Config{Identificar: identificar, Exportador: exp})
	srv := httptest.NewServer(h)
	defer srv.Close()

	const padre = "4bf92f3577b34da6a3ce929d0e0e4736"
	rq, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes/lab/m1/exec", strings.NewReader("{}"))
	rq.Header.Set("Authorization", "Bearer "+tokenSecreto)
	rq.Header.Set("Traceparent", "00-"+padre+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if visto.TraceID != padre || visto.Padre != "00f067aa0ba902b7" {
		t.Errorf("handler saw %+v: the incoming trace was not continued", visto)
	}
	if want := "00-" + padre + "-" + visto.SpanID + "-01"; salida != want {
		t.Errorf("daemon got traceparent %q, want %q", salida, want)
	}
	if got := resp.Header.Get("Traceparent"); !strings.HasPrefix(got, "00-"+padre) {
		t.Errorf("response traceparent = %q", got)
	}
	quedan(t, func() bool { return len(col.spans()) == 1 })
	s := col.spans()[0]
	if s["traceId"] != padre || s["parentSpanId"] != "00f067aa0ba902b7" || s["spanId"] != visto.SpanID {
		t.Errorf("span ids = %v", s)
	}
	for k, want := range map[string]any{
		"http.request.method": "POST", "http.route": "/v1/sandboxes/{id}/exec",
		"http.response.status_code": "201", "kling.tenant": "ana", "kling.sandbox": "lab/m1", "kling.action": "exec",
	} {
		if got := attr(s, k); got != want {
			t.Errorf("attr %s = %v, want %v", k, got, want)
		}
	}
	if s["name"] != "POST /v1/sandboxes/{id}/exec" {
		t.Errorf("name = %v", s["name"])
	}
	if col.auth != "k" {
		t.Errorf("collector headers not sent: %q", col.auth)
	}
	b, _ := json.Marshal(col.trz)
	if strings.Contains(string(b), tokenSecreto) {
		t.Error("the token leaked into the exported spans")
	}
}

func TestSinTraceparentEntranteAbreTrazaYMalFormadoSeIgnora(t *testing.T) {
	for _, v := range []string{"", "basura", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"} {
		tz := Nueva(v)
		if len(tz.TraceID) != 32 || len(tz.SpanID) != 16 || tz.Padre != "" {
			t.Errorf("%q: %+v", v, tz)
		}
	}
}

func TestNoMuestreadaNoSeExporta(t *testing.T) {
	col := nuevoColector(t)
	exp, _ := Nuevo(Opciones{Endpoint: col.URL, Cada: 10 * time.Millisecond})
	h := Medio(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), Config{Exportador: exp})
	rq := httptest.NewRequest("GET", "/v1/templates", nil)
	rq.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	h.ServeHTTP(httptest.NewRecorder(), rq)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/health", nil)) // la sonda no se traza
	time.Sleep(100 * time.Millisecond)
	if n := len(col.spans()); n != 0 {
		t.Errorf("%d span(s) exported", n)
	}
}

func TestColectorCaidoNoBloqueaYCuentaDescartes(t *testing.T) {
	// Un colector que acepta la conexión y no contesta nunca.
	colgado := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-colgado }))
	defer srv.Close()
	defer close(colgado)
	exp, err := Nuevo(Opciones{Endpoint: srv.URL, Cola: 8, Lote: 4, Cada: 5 * time.Millisecond, Plazo: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	h := Medio(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), Config{Exportador: exp})
	ini := time.Now()
	const n = 500
	for i := 0; i < n; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/templates", nil))
	}
	if d := time.Since(ini); d > time.Second {
		t.Errorf("%d requests took %s with the collector hung", n, d)
	}
	quedan(t, func() bool { return exp.Descartados() > 0 })
	if exp.Enviados() != 0 {
		t.Errorf("sent = %d with a dead collector", exp.Enviados())
	}
	// Las cuentas salen también como métrica propia.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	exp.Cerrar(ctx)
	if exp.Descartados() < n-8-4 {
		t.Errorf("dropped = %d, want at least %d", exp.Descartados(), n-12)
	}

	// Y uno que responde 500: el lote se descarta y se cuenta.
	fallo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fallo.Close()
	e2, _ := Nuevo(Opciones{Endpoint: fallo.URL, Cada: 5 * time.Millisecond})
	for i := 0; i < 3; i++ {
		e2.Encolar(Span{Traza: Nueva(""), Nombre: "x", Inicio: time.Now(), Fin: time.Now()})
	}
	quedan(t, func() bool { return e2.Descartados() == 3 })
	// Sin colector en absoluto (puerto cerrado).
	fallo.Close()
	e3, _ := Nuevo(Opciones{Endpoint: fallo.URL, Cada: 5 * time.Millisecond})
	e3.Encolar(Span{Traza: Nueva(""), Nombre: "x", Inicio: time.Now(), Fin: time.Now()})
	quedan(t, func() bool { return e3.Descartados() == 1 })
}

func TestEndpointInvalido(t *testing.T) {
	for _, e := range []string{"", "colector:4318", "ftp://x", "http://"} {
		if _, err := Nuevo(Opciones{Endpoint: e}); err == nil {
			t.Errorf("%q accepted", e)
		}
	}
}

func TestMetricasExportadas(t *testing.T) {
	col := nuevoColector(t)
	const txt = `# HELP kling_sandbox_machines Live sandboxes.
# TYPE kling_sandbox_machines gauge
kling_sandbox_machines{tenant="ana",state="running"} 3
# TYPE kling_sandbox_creations_total counter
kling_sandbox_creations_total{tenant="ana",result="ok"} 7
# TYPE kling_sandbox_create_duration_seconds summary
kling_sandbox_create_duration_seconds{quantile="0.5"} 0.25
kling_sandbox_create_duration_seconds_sum 1.5
kling_sandbox_create_duration_seconds_count 6
kling_sandbox_host_up{host="lab"} 1
`
	exp, _ := Nuevo(Opciones{Endpoint: col.URL, Metricas: func(context.Context) (string, error) { return txt, nil }, IntervaloMetricas: 20 * time.Millisecond})
	defer exp.Cerrar(context.Background())
	quedan(t, func() bool { col.mu.Lock(); defer col.mu.Unlock(); return len(col.met) > 0 })
	col.mu.Lock()
	b, _ := json.Marshal(col.met[0])
	col.mu.Unlock()
	var doc struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []struct {
					Name  string
					Gauge *struct{ DataPoints []map[string]any }
					Sum   *struct {
						DataPoints  []map[string]any
						IsMonotonic bool
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	tipo := map[string]string{}
	for _, m := range doc.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		switch {
		case m.Gauge != nil:
			tipo[m.Name] = "gauge"
		case m.Sum != nil && m.Sum.IsMonotonic:
			tipo[m.Name] = "counter"
		}
	}
	for n, want := range map[string]string{
		"kling_sandbox_machines": "gauge", "kling_sandbox_creations_total": "counter",
		"kling_sandbox_create_duration_seconds": "gauge", "kling_sandbox_create_duration_seconds_sum": "counter",
		"kling_sandbox_create_duration_seconds_count": "counter", "kling_sandbox_host_up": "gauge",
		"kling_sandbox_otlp_spans_dropped_total": "counter",
	} {
		if tipo[n] != want {
			t.Errorf("%s exported as %q, want %q", n, tipo[n], want)
		}
	}
	if !strings.Contains(string(b), `"stringValue":"running"`) {
		t.Errorf("labels lost: %s", b)
	}
}

func TestClasificar(t *testing.T) {
	casos := []struct{ m, p, plantilla, sandbox, accion string }{
		{"POST", "/v1/sandboxes", "/v1/sandboxes", "", "sandbox.create"},
		{"GET", "/v1/sandboxes", "/v1/sandboxes", "", ""},
		{"DELETE", "/v1/sandboxes/lab/m1", "/v1/sandboxes/{id}", "lab/m1", "sandbox.delete"},
		{"POST", "/v1/sandboxes/lab/m1/renew", "/v1/sandboxes/{id}/renew", "lab/m1", "sandbox.renew"},
		{"POST", "/v1/sandboxes/lab/m1/exec", "/v1/sandboxes/{id}/exec", "lab/m1", "exec"},
		{"POST", "/v1/sandboxes/lab/m1/shell", "/v1/sandboxes/{id}/shell", "lab/m1", "shell.open"},
		{"POST", "/v1/sandboxes/lab/m1/endpoints", "/v1/sandboxes/{id}/endpoints", "lab/m1", "endpoint.create"},
		{"GET", "/v1/sandboxes/lab/m1/files", "/v1/sandboxes/{id}/files", "lab/m1", ""},
		{"PUT", "/v1/sandboxes/lab/m1/files", "/v1/sandboxes/{id}/files", "lab/m1", "file.write"},
		{"DELETE", "/v1/sandboxes/lab/m1/files", "/v1/sandboxes/{id}/files", "lab/m1", "file.delete"},
		{"POST", "/v1/sandboxes/lab/m1/processes", "/v1/sandboxes/{id}/processes", "lab/m1", "process.start"},
		{"DELETE", "/v1/sandboxes/lab/m1/processes/12", "/v1/sandboxes/{id}/processes/{pid}", "lab/m1", "process.kill"},
		{"POST", "/v1/sandboxes/lab/m1/processes/12/signal", "/v1/sandboxes/{id}/processes/{pid}/signal", "lab/m1", "process.signal"},
		{"GET", "/v1/sandboxes/lab/m1/processes/12/output", "/v1/sandboxes/{id}/processes/{pid}/output", "lab/m1", ""},
		{"POST", "/v1/sandboxes/lab/m1/sessions", "/v1/sandboxes/{id}/sessions", "lab/m1", "session.create"},
		{"DELETE", "/v1/sandboxes/lab/m1/sessions/s1", "/v1/sandboxes/{id}/sessions/{sid}", "lab/m1", "session.delete"},
		{"POST", "/v1/tenants/ana/keys", "/v1/tenants/{id}/keys", "", "key.create"},
		{"POST", "/v1/tenants/ana/keys/k1/rotate", "/v1/tenants/{id}/keys/{kid}/rotate", "", "key.rotate"},
		{"DELETE", "/v1/tenants/ana/keys/k1", "/v1/tenants/{id}/keys/{kid}", "", "key.revoke"},
		{"GET", "/cualquier/cosa/con/un-id-123", "other", "", ""},
	}
	for _, c := range casos {
		r := Clasificar(c.m, c.p)
		if r.Plantilla != c.plantilla || r.Sandbox != c.sandbox || r.Accion != c.accion {
			t.Errorf("%s %s = %+v", c.m, c.p, r)
		}
	}
}

func TestHijackYFlushPasan(t *testing.T) {
	h := Medio(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("no Flusher")
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
		c.Close()
	}), Config{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/sandboxes/lab/m1/shell")
	if err == nil {
		resp.Body.Close()
	}
}
