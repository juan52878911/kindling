package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// call es una llamada al kling falso: argv y stdin, o un SetLabels.
type call struct {
	args   []string
	stdin  string
	ref    string            // SetLabels
	labels map[string]string // SetLabels
}

// fakeKling simula lo justo del daemon: plantillas, máquinas con etiquetas,
// run -from, sandbox fork, exec (pg_isready y el psql de la rotación), thaw,
// rm y SetLabels. Apunta cada llamada.
type fakeKling struct {
	mu        sync.Mutex
	calls     []call
	snaps     map[string]*api.Snapshot
	machines  map[string]*api.Machine // por id
	verifier  map[string]string       // id -> verificador guardado en "pg_authid"
	seq       int
	rotations int
	// failRotation hace fallar la rotación número n (desde 1); 0 = ninguna.
	failRotation int
	// wrongVerifier: la rotación "funciona" pero pg_authid no lo refleja.
	wrongVerifier bool
	// pgDown: pg_isready no contesta nunca.
	pgDown bool
	// macOS: las máquinas se alcanzan por reenvíos en loopback.
	macOS bool
	// roRoles: roles de kling db role que hay "dentro" de cada máquina (por id)
	// y de cada plantilla (por nombre). Un fork y un run -from los heredan,
	// como heredan la RAM y el disco; la purga de prepare los quita.
	roRoles   map[string][]string
	snapRoles map[string][]string
	// purged: ids en los que se purgaron los roles (SQL) y pg_hba (sh), en orden.
	purged, purgedHBA []string
	// purgeLeaves: la purga "no puede" y contesta que queda uno.
	purgeLeaves bool
	// creds: credenciales entregadas por SetCredential, por id de máquina.
	// removed: "id env upstream" de cada RemoveCredential.
	creds   map[string][]api.CredentialSpec
	removed []string
	// freezeFails: kling freeze siempre falla.
	freezeFails bool
	// credErr, si no es nil, lo devuelve SetCredential.
	credErr error
	// graphs: los grafos que crea "graph up"; graphSecrets: la clave que llegó
	// por stdin de cada uno (para comprobar que no viaja por otro lado).
	graphs       map[string]*api.Graph
	graphSecrets map[string]string
	graphFiles   []string // contenido de cada fichero de grafo recibido
	graphUpFails bool
	graphLsFails error // si no es nil, "graph ls" falla con él
}

func newFake() *fakeKling {
	return &fakeKling{
		snaps: map[string]*api.Snapshot{
			"pg": {Name: "pg", Labels: map[string]string{api.LabelPorts: "8080"}},
		},
		machines:  map[string]*api.Machine{},
		verifier:  map[string]string{},
		roRoles:   map[string][]string{},
		snapRoles: map[string][]string{},
	}
}

func (f *fakeKling) find(ref string) *api.Machine {
	if mc, ok := f.machines[ref]; ok {
		return mc
	}
	for _, mc := range f.machines {
		if mc.Name == ref {
			return mc
		}
	}
	return nil
}

func (f *fakeKling) newMachine(name string, labels map[string]string) *api.Machine {
	f.seq++
	id := fmt.Sprintf("%016x", 0xdb0000+f.seq)
	if name == "" {
		name = "m" + strconv.Itoa(f.seq)
	}
	mc := &api.Machine{ID: id, Name: name, State: api.StateRunning, Labels: labels,
		IP: fmt.Sprintf("10.200.%d.2", f.seq)}
	if f.macOS {
		mc.IP = "172.16.0.2"
		mc.Forwards = map[string]string{"10000": "127.0.0.1:29001"}
		for _, p := range strings.Split(labels[api.LabelPorts], ",") {
			if p == "5432" {
				mc.Forwards["5432"] = fmt.Sprintf("127.0.0.1:%d", 29100+f.seq)
			}
		}
	}
	f.machines[id] = mc
	return mc
}

func clone(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (f *fakeKling) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{args: append([]string(nil), args...), stdin: string(in)})
	fail := func(format string, a ...any) ([]byte, error) { return nil, fmt.Errorf(format, a...) }

	switch {
	case len(args) >= 3 && args[0] == "template" && args[1] == "inspect":
		s, ok := f.snaps[args[2]]
		if !ok {
			return fail("no template %s", args[2])
		}
		return json.Marshal(s)

	case args[0] == "run":
		var from, name string
		labels := map[string]string{}
		for i := 1; i < len(args)-1; i++ {
			switch args[i] {
			case "-from":
				from = args[i+1]
			case "-name":
				name = args[i+1]
			case "-label":
				k, v, _ := strings.Cut(args[i+1], "=")
				labels[k] = v
			}
		}
		s, ok := f.snaps[from]
		if !ok {
			return fail("no template %s", from)
		}
		if f.find(name) != nil {
			return fail("name %s in use", name)
		}
		merged := clone(s.Labels)
		for k, v := range labels {
			merged[k] = v
		}
		mc := f.newMachine(name, merged)
		mc.From = from
		f.roRoles[mc.ID] = append([]string(nil), f.snapRoles[from]...)
		return []byte(mc.ID[:12] + "  " + mc.Name + "  instantiated\n"), nil

	case args[0] == "inspect":
		mc := f.find(args[1])
		if mc == nil {
			return fail("machine %q doesn't exist", args[1])
		}
		return json.Marshal(mc)

	case args[0] == "exec":
		i := indexOf(args, "--")
		if i < 2 {
			return fail("bad exec %v", args)
		}
		mc := f.find(args[i-1])
		if mc == nil {
			return fail("no machine %s", args[i-1])
		}
		cmd := strings.Join(args[i+1:], " ")
		switch {
		case strings.Contains(string(in), purgeMarker) && strings.HasSuffix(cmd, "sh -s"):
			f.purgedHBA = append(f.purgedHBA, mc.ID)
			return nil, nil
		case strings.Contains(string(in), purgeMarker):
			f.purged = append(f.purged, mc.ID)
			if f.purgeLeaves {
				return []byte("0\n1\n"), nil
			}
			delete(f.roRoles, mc.ID)
			return []byte("0\n0\n"), nil
		case strings.Contains(cmd, "pg_isready"):
			if f.pgDown {
				return fail("exit status 2")
			}
			return nil, nil
		case strings.Contains(cmd, "psql"):
			f.rotations++
			if f.rotations == f.failRotation {
				return fail("psql: ERROR: ... ALTER ROLE app PASSWORD 'SCRAM...'")
			}
			v := verifierIn(string(in))
			if v == "" {
				return fail("no verifier in %q", in)
			}
			if f.wrongVerifier {
				return []byte("f\n"), nil
			}
			f.verifier[mc.ID] = v
			return []byte("t\n"), nil
		}
		return nil, nil

	case len(args) >= 2 && args[0] == "sandbox" && args[1] == "fork":
		src := f.find(args[2])
		if src == nil || src.Labels[api.LabelKind] != api.KindSandbox {
			return fail("no sandbox %q", args[2])
		}
		if src.State != api.StateRunning {
			return fail("only a running machine can be forked")
		}
		n, _ := strconv.Atoi(args[indexOf(args, "-n")+1])
		res := api.ForkResult{Snapshot: "fork-x"}
		for j := 0; j < n; j++ {
			l := clone(src.Labels)
			l["kling.fork-of"] = src.ID
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-label" {
					if k, v, ok := strings.Cut(args[i+1], "="); ok {
						l[k] = v
					}
				}
			}
			mc := f.newMachine("", l)
			f.verifier[mc.ID] = f.verifier[src.ID]
			f.roRoles[mc.ID] = append([]string(nil), f.roRoles[src.ID]...)
			res.Sandboxes = append(res.Sandboxes, mc)
		}
		return json.Marshal(res)

	case args[0] == "graph":
		return f.graphCmd(args[1:], string(in))

	case args[0] == "ps":
		ids := make([]string, 0, len(f.machines))
		for id := range f.machines {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		list := make([]*api.Machine, 0, len(ids))
		for _, id := range ids {
			list = append(list, f.machines[id])
		}
		return json.Marshal(list)

	case args[0] == "freeze":
		mc := f.find(args[1])
		if mc == nil {
			return fail("no machine")
		}
		if f.freezeFails {
			return fail("freeze failed")
		}
		mc.State = api.StateWarm
		return nil, nil

	case args[0] == "thaw":
		mc := f.find(args[1])
		if mc == nil {
			return fail("no machine")
		}
		mc.State = api.StateRunning
		return nil, nil

	case args[0] == "rm":
		mc := f.find(args[len(args)-1])
		if mc == nil {
			return fail("no machine %s", args[len(args)-1])
		}
		delete(f.machines, mc.ID)
		delete(f.verifier, mc.ID)
		return nil, nil
	}
	return fail("fake kling: unexpected %v", args)
}

func (f *fakeKling) SetLabels(_ context.Context, ref string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{ref: ref, labels: clone(labels)})
	mc := f.find(ref)
	if mc == nil {
		return errors.New("no machine")
	}
	for k, v := range labels {
		mc.Labels[k] = v
	}
	return nil
}

func (f *fakeKling) SetCredential(_ context.Context, ref string, spec api.CredentialSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.credErr != nil {
		return f.credErr
	}
	mc := f.find(ref)
	if mc == nil {
		return errors.New("no machine")
	}
	if f.creds == nil {
		f.creds = map[string][]api.CredentialSpec{}
	}
	f.creds[mc.ID] = append(f.creds[mc.ID], spec)
	return nil
}

func (f *fakeKling) RemoveCredential(_ context.Context, ref, env, upstream string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	mc := f.find(ref)
	if mc == nil {
		return errors.New("no machine")
	}
	f.removed = append(f.removed, mc.ID+" "+env+" "+upstream)
	return nil
}

func indexOf(s []string, x string) int {
	for i, v := range s {
		if v == x {
			return i
		}
	}
	return -1
}

// verifierIn saca el verificador del ALTER ROLE que llegó por stdin.
func verifierIn(sql string) string {
	_, rest, ok := strings.Cut(sql, "PASSWORD '")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, "'")
	return v
}

// testApp monta un app contra el falso, con el estado en un directorio temporal.
type testApp struct {
	*app
	f        *fakeKling
	out, err *bytes.Buffer
	tty      bool
	psql     []string // entorno + argumentos de la última llamada a psql
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	t.Setenv("KLING_DB_STATE", t.TempDir())
	f := newFake()
	ta := &testApp{f: f, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	ta.app = &app{
		k: f, stdout: ta.out, stderr: ta.err, stdin: strings.NewReader(""),
		stdoutTTY: func() bool { return ta.tty },
		runPsql: func(_ context.Context, env, args []string) error {
			ta.psql = append(append([]string(nil), env...), args...)
			return nil
		},
		sleep:     func(time.Duration) {},
		readyWait: 0,
	}
	return ta
}
