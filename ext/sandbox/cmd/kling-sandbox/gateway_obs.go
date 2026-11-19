package main

// Observabilidad del gateway: trazas y métricas OTLP (internal/telemetria) y
// auditoría por inquilino (internal/auditoria). Se engancha como middleware
// alrededor del handler del frontal; si no se pide nada, el handler se
// devuelve tal cual y las peticiones no pagan nada.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/auditoria"
	"github.com/juan52878911/kindling/ext/sandbox/internal/frontal"
	"github.com/juan52878911/kindling/ext/sandbox/internal/telemetria"
)

type banderasObs struct {
	endpoint  *string
	intervalo *time.Duration
	auditDir  *string
	auditMiB  *int
	auditKeep *int
}

func banderasObservabilidad(fs *flag.FlagSet) banderasObs {
	return banderasObs{
		endpoint: fs.String("otlp-endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
			"OTLP/HTTP collector base URL (http://host:4318) to export traces and metrics as JSON; empty = off (default OTEL_EXPORTER_OTLP_ENDPOINT)"),
		intervalo: fs.Duration("otlp-metrics-interval", telemetria.MetricasPorDefecto, "how often /v1/metrics is exported to the OTLP collector"),
		auditDir: fs.String("audit-dir", os.Getenv("KLING_SANDBOX_AUDIT_DIR"),
			"directory for the per-tenant audit log (audit.jsonl, 0600, rotated); empty = audit off. Served at GET /v1/audit"),
		auditMiB:  fs.Int("audit-max-mib", 0, "size in MiB at which the audit log rotates (default 8)"),
		auditKeep: fs.Int("audit-keep", 0, "rotated audit files to keep (default 5)"),
	}
}

// cabecerasOTLP lee OTEL_EXPORTER_OTLP_HEADERS ("k=v,k2=v2"), el medio
// estándar de dar al colector un token de acceso.
func cabecerasOTLP() map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && k != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// observabilidad envuelve h según las banderas. cerrar vacía el exportador y
// el registro; es seguro llamarlo siempre.
func observabilidad(h http.Handler, identificar func(string) (frontal.Identidad, bool),
	metricas func(context.Context) (string, error), b banderasObs) (http.Handler, func(), error) {
	if *b.endpoint == "" && *b.auditDir == "" {
		return h, func() {}, nil
	}
	cfg := telemetria.Config{Identificar: func(a string) (telemetria.Identidad, bool) {
		i, ok := identificar(a)
		return telemetria.Identidad{Nombre: i.Nombre, Admin: i.Admin}, ok
	}}
	var exp *telemetria.Exportador
	var reg *auditoria.Registro
	if *b.endpoint != "" {
		var err error
		exp, err = telemetria.Nuevo(telemetria.Opciones{
			Endpoint: *b.endpoint, Cabeceras: cabecerasOTLP(), Metricas: metricas, IntervaloMetricas: *b.intervalo,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("-otlp-endpoint: %w", err)
		}
		cfg.Exportador = exp
		log.Printf("OTLP export to %s (traces + metrics every %s)", *b.endpoint, *b.intervalo)
	}
	if *b.auditDir != "" {
		var err error
		reg, err = auditoria.Abrir(auditoria.Opciones{
			Dir: *b.auditDir, MaxBytes: int64(*b.auditMiB) << 20, Generaciones: *b.auditKeep,
		})
		if err != nil {
			if exp != nil {
				exp.Cerrar(context.Background())
			}
			return nil, nil, fmt.Errorf("-audit-dir: %w", err)
		}
		cfg.Observadores = append(cfg.Observadores, auditoria.Observador(reg))
		cfg.Propios = auditoria.Servir(reg)
		log.Printf("audit log in %s", *b.auditDir)
	}
	return telemetria.Medio(h, cfg), func() {
		if exp != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			exp.Cerrar(ctx)
			cancel()
		}
		if reg != nil {
			reg.Cerrar()
		}
	}, nil
}
