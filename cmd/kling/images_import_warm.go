package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// reTemplateWarm: el nombre de la plantilla de -warm. Acaba en el nombre de la
// máquina de preparación, así que lleva el contorno de un nombre de imagen.
var reTemplateWarm = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,40}$`)

// warmOps es lo que el encadenado necesita del daemon; *api.Client lo cumple y
// el test lo sustituye por un doble.
type warmOps interface {
	Run(ctx context.Context, r api.RunRequest) (*api.Machine, error)
	Exec(ctx context.Context, ref string, r api.ExecRequest, onOutput func(stream string, data []byte) error) (*api.ExecResult, error)
	CommitWith(ctx context.Context, ref string, req api.CommitRequest) (*api.Snapshot, error)
	Remove(ctx context.Context, ref string) error
}

// warmSpec son las opciones de `image import -warm`.
type warmSpec struct {
	Image, Template string
	Steps           []string
	CPUs, MemMiB    int
	Egress          string
	Replace         bool
	Wait            time.Duration // espera a "listo" al arrancar y al guardar
	StepTimeout     time.Duration
}

// warmUp lleva una imagen ya importada a una plantilla despierta: arranca una
// máquina de preparación a CPU completa y espera a que su servicio esté listo
// por la sonda de la imagen, ejecuta los pasos dentro (sh -c, en orden; el
// primero que falla aborta), guarda la máquina viva como plantilla (el daemon
// espera otra vez a "listo": un paso puede haber reiniciado el servicio) y
// retira la máquina de preparación. La salida de los pasos va a out.
func warmUp(ctx context.Context, ops warmOps, w warmSpec, out io.Writer, logf func(string, ...any)) (*api.Snapshot, error) {
	m := w.Template + "-warm"
	logf("starting %s from %s (%d vCPU, %d MiB, full CPU)...", m, w.Image, w.CPUs, w.MemMiB)
	mc, err := ops.Run(ctx, api.RunRequest{
		Name: m, Image: w.Image, VCPUs: w.CPUs, MemMiB: w.MemMiB,
		CPUPct: w.CPUs * 100, AllowExec: true, Egress: w.Egress,
		WaitReady: true, ReadyTimeoutSeconds: int(w.Wait.Seconds()),
	})
	if err != nil {
		return nil, fmt.Errorf("starting %s: %w", m, err)
	}
	// Se retira siempre, por éxito o por fallo, y aunque se cancele el contexto.
	defer func() { _ = ops.Remove(context.WithoutCancel(ctx), m) }()
	if mc.Ready == api.ReadyWaiting || mc.Ready == api.ReadyFailed {
		return nil, fmt.Errorf("%s is running but not ready (%s): see kling logs %s", m, mc.Ready, m)
	}
	for i, step := range w.Steps {
		logf("step %d/%d: %s", i+1, len(w.Steps), step)
		res, err := ops.Exec(ctx, m, api.ExecRequest{
			Cmd: []string{"sh", "-c", step}, TimeoutSeconds: int(w.StepTimeout.Seconds()),
		}, func(stream string, data []byte) error {
			_, err := out.Write(data)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		switch {
		case res.TimedOut:
			return nil, fmt.Errorf("step %d timed out after %s (-step-timeout)", i+1, w.StepTimeout)
		case res.ExitCode != 0:
			return nil, fmt.Errorf("step %d exited with status %d", i+1, res.ExitCode)
		}
	}
	logf("saving %s (waits for the service to be ready)...", w.Template)
	return ops.CommitWith(ctx, m, api.CommitRequest{
		Name: w.Template, Replace: w.Replace, ReadyTimeoutSeconds: int(w.Wait.Seconds()),
	})
}
