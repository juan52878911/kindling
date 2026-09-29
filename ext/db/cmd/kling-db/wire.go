package main

// La costura con los paquetes de diagnóstico, que escriben otros con este
// contrato exacto:
//
//	doctor.Run(ctx, k, doctor.Target{Machine, URL}, w) (problems int, err error)
//	dbaudit.RunEngine(ctx, k, machine, engine, since, jsonOut, w) error

import (
	"context"
	"io"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbaudit"
	"github.com/juan52878911/kindling/ext/db/internal/doctor"
	"github.com/juan52878911/kindling/ext/db/internal/klingc"
)

// doctorTarget es lo que se diagnostica: una copia o una URL.
type doctorTarget struct{ Machine, URL string }

func runDoctor(ctx context.Context, k klingc.Kling, t doctorTarget, w io.Writer) (int, error) {
	return doctor.Run(ctx, k, doctor.Target{Machine: t.Machine, URL: t.URL}, w)
}

func runAudit(ctx context.Context, k klingc.Kling, machine, engine string, since time.Duration, jsonOut bool, w io.Writer) error {
	return dbaudit.RunEngine(ctx, k, machine, engine, since, jsonOut, w)
}
