package main

// Ampliación del Kling falso para las operaciones de la fase 3 (rehearse,
// snapshot, undo). Envuelve a fakeKling: lo que no conoce lo delega.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
)

type opsFake struct {
	*fakeKling
	omu sync.Mutex
	// dbSizes: tamaño de la "base" por id de máquina.
	dbSizes map[string]int64
	// applied: lo que recibió cada máquina como migración, en orden.
	applied map[string][]string
	// clients: conexiones de cliente abiertas (todas las máquinas).
	clients int
	// migrating: hay una migración en curso (para el muestreo de locks).
	migrating bool
	// waiters: lo que ve el muestreo mientras hay una migración en curso.
	waiters int
	// saveTick da fecha creciente a las plantillas.
	saveTick int
	// failSave hace fallar kling save.
	failSave bool
	// alterFail: los n primeros ALTER ROLE de rotación fallan tras aplicarse o no.
	alterCalls int
	failAlter  map[int]bool // número de llamada (desde 1) -> falla
}

func (o *opsFake) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	rewind := func() io.Reader { return strings.NewReader(string(in)) }
	cmd := ""
	if i := indexOf(args, "--"); args[0] == "exec" && i >= 0 {
		cmd = strings.Join(args[i+1:], " ")
	}
	id := ""
	if cmd != "" {
		id = args[indexOf(args, "--")-1]
	}
	sql := string(in)

	switch {
	case args[0] == "exec" && strings.Contains(cmd, "PGOPTIONS"):
		o.fakeKling.mu.Lock()
		o.fakeKling.calls = append(o.fakeKling.calls, call{args: append([]string(nil), args...), stdin: sql})
		mc := o.fakeKling.find(id)
		o.fakeKling.mu.Unlock()
		if mc == nil {
			return nil, fmt.Errorf("no machine %s", id)
		}
		o.omu.Lock()
		o.migrating = true
		o.applied[mc.ID] = append(o.applied[mc.ID], sql)
		o.omu.Unlock()
		defer func() { o.omu.Lock(); o.migrating = false; o.omu.Unlock() }()
		if strings.Contains(sql, "-- SLOW") {
			time.Sleep(40 * time.Millisecond)
		}
		if strings.Contains(sql, "-- LOCK") {
			return nil, &klingc.Error{Args: args, Err: fmt.Errorf("exit status 3"),
				Stderr: "psql:<stdin>:1: ERROR:  canceling statement due to lock timeout\nLINE 1: ALTER TABLE secretos ..."}
		}
		if strings.Contains(sql, "-- BAD") {
			return nil, &klingc.Error{Args: args, Err: fmt.Errorf("exit status 3"),
				Stderr: "psql:<stdin>:1: ERROR:  syntax error at or near \"BOOM\"\nLINE 1: BOOM 'clave-secreta'"}
		}
		o.omu.Lock()
		if strings.Contains(sql, "-- GROW") {
			o.dbSizes[mc.ID] += 1 << 20
		}
		o.omu.Unlock()
		return nil, nil

	case args[0] == "exec" && strings.Contains(sql, "pg_database_size"):
		o.fakeKling.mu.Lock()
		o.fakeKling.calls = append(o.fakeKling.calls, call{args: append([]string(nil), args...), stdin: sql})
		mc := o.fakeKling.find(id)
		o.fakeKling.mu.Unlock()
		if mc == nil {
			return nil, fmt.Errorf("no machine %s", id)
		}
		o.omu.Lock()
		defer o.omu.Unlock()
		if _, ok := o.dbSizes[mc.ID]; !ok {
			o.dbSizes[mc.ID] = 8 << 20
		}
		return []byte(fmt.Sprintf("%d\n", o.dbSizes[mc.ID])), nil

	case args[0] == "exec" && strings.Contains(sql, "wait_event_type"):
		o.omu.Lock()
		defer o.omu.Unlock()
		if o.migrating {
			return []byte(fmt.Sprintf("%d\n", o.waiters)), nil
		}
		return []byte("0\n"), nil

	case args[0] == "exec" && strings.Contains(sql, "backend_type = 'client backend'"):
		o.omu.Lock()
		defer o.omu.Unlock()
		return []byte(fmt.Sprintf("%d\n", o.clients)), nil

	case args[0] == "exec" && strings.HasPrefix(sql, "CHECKPOINT"):
		o.fakeKling.mu.Lock()
		o.fakeKling.calls = append(o.fakeKling.calls, call{args: append([]string(nil), args...), stdin: sql})
		o.fakeKling.mu.Unlock()
		return nil, nil

	case args[0] == "exec" && strings.Contains(sql, "ALTER ROLE"):
		o.omu.Lock()
		o.alterCalls++
		fail := o.failAlter[o.alterCalls]
		o.omu.Unlock()
		if fail {
			// Simula un plazo vencido: la base SÍ aplicó el cambio.
			o.fakeKling.Run(ctx, rewind(), args...)
			return nil, fmt.Errorf("context deadline exceeded")
		}

	case args[0] == "save":
		o.fakeKling.mu.Lock()
		defer o.fakeKling.mu.Unlock()
		o.fakeKling.calls = append(o.fakeKling.calls, call{args: append([]string(nil), args...)})
		if o.failSave {
			return nil, fmt.Errorf("save failed")
		}
		mc := o.fakeKling.find(args[len(args)-2])
		name := args[len(args)-1]
		if mc == nil || mc.State != api.StateRunning {
			return nil, fmt.Errorf("only a running machine can be committed")
		}
		if _, ok := o.fakeKling.snaps[name]; ok {
			return nil, fmt.Errorf("snapshot %q already exists", name)
		}
		o.saveTick++
		o.fakeKling.snaps[name] = &api.Snapshot{Name: name, Labels: clone(mc.Labels),
			CreatedAt: time.Unix(1700000000+int64(o.saveTick), 0).UTC(), DiskBytes: 5 << 20, MemBytes: 256 << 20}
		return []byte(name + "  template\n"), nil

	case len(args) >= 3 && args[0] == "template" && args[1] == "ls":
		o.fakeKling.mu.Lock()
		defer o.fakeKling.mu.Unlock()
		o.fakeKling.calls = append(o.fakeKling.calls, call{args: append([]string(nil), args...)})
		list := []*api.Snapshot{}
		for _, s := range o.fakeKling.snaps {
			c := *s
			c.Instances = 0
			for _, m := range o.fakeKling.machines {
				if m.From == s.Name {
					c.Instances++
				}
			}
			list = append(list, &c)
		}
		return json.Marshal(list)

	case len(args) >= 3 && args[0] == "template" && args[1] == "rm":
		o.fakeKling.mu.Lock()
		defer o.fakeKling.mu.Unlock()
		o.fakeKling.calls = append(o.fakeKling.calls, call{args: append([]string(nil), args...)})
		name := args[len(args)-1]
		if _, ok := o.fakeKling.snaps[name]; !ok {
			return nil, fmt.Errorf("no template %s", name)
		}
		for _, m := range o.fakeKling.machines {
			if m.From == name {
				return nil, fmt.Errorf("template %s has live instances", name)
			}
		}
		delete(o.fakeKling.snaps, name)
		return nil, nil
	}
	return o.fakeKling.Run(ctx, rewind(), args...)
}

type opsApp struct {
	*testApp
	o *opsFake
}

func newOpsApp(t *testing.T) *opsApp {
	t.Helper()
	ta := newTestApp(t)
	o := &opsFake{fakeKling: ta.f, dbSizes: map[string]int64{}, applied: map[string][]string{}, failAlter: map[int]bool{}}
	ta.app.k = o
	lockSampleEvery = 0
	t.Cleanup(func() { lockSampleEvery = 500 * time.Millisecond })
	return &opsApp{testApp: ta, o: o}
}

// pg devuelve una copia lista (up) con contraseña propia.
func (oa *opsApp) copyReady(t *testing.T, name string) *api.Machine {
	t.Helper()
	mc, err := oa.up(ctx, "pg", name, 0, "local")
	if err != nil {
		t.Fatal(err)
	}
	return mc
}
