package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/dorado"
	"github.com/juan52878911/kindling/pkg/api"
)

// `kling ai model roll <modelo> [-to n]`: pone a servir otra versión del dorado
// de un modelo (R8-06a). Apunta el bloque de ai.json al dorado <modelo>-v<n>,
// pide al gateway que relea el registro y, cuando las réplicas del dorado
// viejo dejan de trabajar, las borra. Antes, rehacer un dorado exigía borrar a
// mano las réplicas (el daemon no reemplaza un snapshot con copias vivas); con
// versiones, el dorado nuevo convive con el viejo y nada se corta.
//
// El dorado viejo se queda: `-to` con su versión es la vuelta atrás.

type rollOptions struct {
	Model    string
	To       int // 0 = la versión más alta
	Config   string
	NoReload bool
	// Drain es cuánto esperar a que una réplica del dorado viejo que sigue
	// corriendo (con peticiones en vuelo) quede ociosa antes de borrarla igual.
	Drain time.Duration
	// Poll es el intervalo con que se mira el estado de esas réplicas.
	Poll time.Duration
}

type rollResult struct {
	From, To string
	Removed  []string // réplicas del dorado viejo borradas
}

func modelsRoll(args []string) error {
	fs := flag.NewFlagSet("ai model roll", flag.ContinueOnError)
	host := hostFlag(fs)
	gw := aiClientFlags(fs)
	cfgPath := fs.String("config", aiDefault("ai.json"), "model and task registry")
	to := fs.String("to", "", "version to serve: 3 or v3 (default: the newest golden snapshot of the model)")
	noReload := fs.Bool("no-reload", false, "only rewrite ai.json: do not reload the gateway nor remove the old replicas")
	drain := fs.Duration("drain", 30*time.Second, "how long a running replica of the old version may keep serving before it is removed anyway")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUso("usage: kling ai model roll <model> [-to n] [-drain 30s] [-no-reload]")
	}
	o := rollOptions{Model: fs.Arg(0), Config: *cfgPath, NoReload: *noReload, Drain: *drain, Poll: 500 * time.Millisecond}
	if *to != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(*to, "v"))
		if err != nil || n < 1 {
			return fmt.Errorf("-to %q: a version number such as 3", *to)
		}
		o.To = n
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	var reload func(context.Context) error
	if !o.NoReload {
		reload = func(context.Context) error {
			c, err := gw()
			if err != nil {
				return err
			}
			return c.do(http.MethodPost, "/v1/admin/reload", nil, nil)
		}
	}
	r, err := modelRoll(ctx, api.NewClient(hostOf(*host)), reload, o)
	if err != nil {
		return err
	}
	if r.From == r.To {
		fmt.Printf("%s already serves %s\n", o.Model, r.To)
		return nil
	}
	fmt.Printf(uiMarks().ok+" %s now serves %s (was %s)\n", o.Model, r.To, r.From)
	if o.NoReload {
		fmt.Println("ai.json rewritten; run kling ai reload, then remove the old replicas (kling ps)")
		return nil
	}
	if len(r.Removed) > 0 {
		fmt.Printf("  removed %d replica(s) of %s\n", len(r.Removed), r.From)
	}
	if n, ok := dorado.ParseVersion(o.Model, r.From); ok {
		fmt.Printf("  %s is kept to roll back:  kling ai model roll %s -to %d\n", r.From, o.Model, n)
	} else {
		fmt.Printf("  %s is kept: to go back, set \"snapshot\": %q in %s and run kling ai reload\n", r.From, r.From, o.Config)
	}
	return nil
}

// modelRoll hace el cambio. reload nil = no recargar ni drenar (-no-reload).
// Si la recarga falla, ai.json vuelve a como estaba: el gateway sigue con el
// dorado viejo y el fichero no debe decir otra cosa.
func modelRoll(ctx context.Context, c *api.Client, reload func(context.Context) error, o rollOptions) (*rollResult, error) {
	reg, err := readRegistry(o.Config)
	if err != nil {
		return nil, err
	}
	if reg.orig == nil {
		return nil, fmt.Errorf("%s does not exist", o.Config)
	}
	raw, ok := reg.models[o.Model]
	if !ok {
		return nil, fmt.Errorf("%s: no model %q", o.Config, o.Model)
	}
	var block map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("%s: model %q: %w", o.Config, o.Model, err)
	}
	var cur string
	if s, ok := block["snapshot"]; ok {
		_ = json.Unmarshal(s, &cur)
	}
	if cur == "" {
		return nil, fmt.Errorf("%s: model %q has no golden snapshot to roll (an upstream backend is not restored by kindling)", o.Config, o.Model)
	}

	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	var target *api.Snapshot
	top := 0
	for _, s := range snaps {
		n, ok := dorado.ParseVersion(o.Model, s.Name)
		if !ok {
			continue
		}
		if (o.To == 0 && n > top) || n == o.To {
			target, top = s, n
		}
	}
	if target == nil {
		if o.To != 0 {
			return nil, fmt.Errorf("no golden snapshot %s on the daemon", dorado.Versioned(o.Model, o.To))
		}
		return nil, fmt.Errorf("no versioned golden snapshot of %s on the daemon (kling ai model deploy %s ...)", o.Model, o.Model)
	}
	res := &rollResult{From: cur, To: target.Name}
	if cur == target.Name {
		return res, nil
	}

	// Antes de que el gateway lo sirva: la primera réplica del dorado nuevo
	// no tiene que leerlo del disco (C9-10).
	precalentar(ctx, c, target.Name)
	block["snapshot"], _ = json.Marshal(target.Name)
	// El puerto lo dice el dorado (kling.ports, el que deja pasar el proxy):
	// una versión nueva puede escuchar en otro.
	if p, err := strconv.Atoi(target.Labels[api.LabelPorts]); err == nil && p > 0 && p < 65536 {
		block["port"], _ = json.Marshal(p)
	}
	if reg.models[o.Model], err = json.Marshal(block); err != nil {
		return nil, err
	}
	if err := reg.write(); err != nil {
		return nil, err
	}
	if reload == nil {
		return res, nil
	}
	if err := reload(ctx); err != nil {
		if rerr := reg.restore(); rerr != nil {
			return nil, fmt.Errorf("reloading the gateway: %w (and restoring %s: %v)", err, o.Config, rerr)
		}
		return nil, fmt.Errorf("reloading the gateway: %w (%s left as it was)", err, o.Config)
	}
	res.Removed, err = drainReplicas(ctx, c, cur, o.Drain, o.Poll)
	return res, err
}

// drainReplicas borra las réplicas restauradas del dorado old. Las que no
// corren (congeladas, pausadas, paradas) no tienen nada en vuelo y se borran
// ya; las que corren pueden estar contestando la última petición que entró
// antes de la recarga, así que se espera a que el gateway las deje ociosas, o
// hasta drain, y se borran igual.
func drainReplicas(ctx context.Context, c *api.Client, old string, drain, poll time.Duration) ([]string, error) {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(drain)
	var removed []string
	var errs []error
	for {
		ms, err := c.List(ctx)
		if err != nil {
			return removed, err
		}
		running := 0
		for _, m := range ms {
			// Solo las restauradas de old: nunca algo que no salió de ese dorado.
			if m.From != old {
				continue
			}
			if m.State == api.StateRunning && time.Now().Before(deadline) {
				running++
				continue
			}
			if err := c.Remove(ctx, m.ID); err != nil {
				errs = append(errs, fmt.Errorf("removing replica %s: %w", m.Name, err))
				continue
			}
			removed = append(removed, m.Name)
		}
		if running == 0 || len(errs) > 0 {
			return removed, errors.Join(errs...)
		}
		select {
		case <-ctx.Done():
			return removed, ctx.Err()
		case <-time.After(poll):
		}
	}
}
