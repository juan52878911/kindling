package aigw

import (
	"testing"
	"time"
)

// escritorRetenido es un lector de /metrics que no lee: su primer Write se
// queda esperando hasta que se cierre soltar.
type escritorRetenido struct {
	entro  chan struct{}
	soltar chan struct{}
	una    bool
}

func (e *escritorRetenido) Write(p []byte) (int, error) {
	if !e.una {
		e.una = true
		close(e.entro)
		<-e.soltar
	}
	return len(p), nil
}

// Un cliente lento de /metrics no puede retener el candado de las métricas:
// lo toman todas las peticiones para contar, y se quedarían esperando a él.
func TestMetricsLectorLentoNoRetieneElCandado(t *testing.T) {
	m := newMetrics()
	m.addInflight(1)
	e := &escritorRetenido{entro: make(chan struct{}), soltar: make(chan struct{})}
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		m.write(e, chispaStats{}, nil, nil, learnSnap{})
	}()
	<-e.entro
	contado := make(chan struct{})
	go func() {
		m.addInflight(1)
		close(contado)
	}()
	select {
	case <-contado:
	case <-time.After(2 * time.Second):
		t.Error("contar una petición esperó a que el lector de /metrics leyera")
	}
	close(e.soltar)
	<-hecho
	<-contado
}

// /metrics enseña nombres y volúmenes de todas las tareas: es del operador,
// no de un inquilino.
func TestMetricsSoloTokenPrincipal(t *testing.T) {
	g, _, _ := newTestGateway(t, func(c *Config) {
		c.Tenants = []TenantConfig{{Name: "bot", Token: "tenant-token-0123456789"}}
	})
	h := g.Handler("main-token-0123456789")
	if rec := do(t, h, "GET", "/metrics", "tenant-token-0123456789", nil); rec.Code != 403 {
		t.Fatalf("inquilino en /metrics: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/metrics", "main-token-0123456789", nil); rec.Code != 200 {
		t.Fatalf("token principal en /metrics: %d", rec.Code)
	}
}
