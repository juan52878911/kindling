package guest

import (
	"errors"
	"slices"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Las reglas de Docker: command cambia los argumentos, entrypoint el
// ejecutable (y vacía los argumentos si no hay command).
func TestMachineArgv(t *testing.T) {
	img := &api.ServiceSpec{Argv: []string{"docker-entrypoint.sh", "redis-server"},
		Entrypoint: []string{"docker-entrypoint.sh"}, Cmd: []string{"redis-server"}}
	casos := []struct {
		nombre string
		spec   *api.ServiceSpec
		c      api.MachineCmd
		want   []string
	}{
		{"cmd", img, api.MachineCmd{Cmd: []string{"redis-server", "--port", "7000"}},
			[]string{"docker-entrypoint.sh", "redis-server", "--port", "7000"}},
		{"entrypoint", img, api.MachineCmd{Entrypoint: []string{"sh", "-c"}}, []string{"sh", "-c"}},
		{"los dos", img, api.MachineCmd{Entrypoint: []string{"sh", "-c"}, Cmd: []string{"echo hola"}},
			[]string{"sh", "-c", "echo hola"}},
		{"sin servicio", nil, api.MachineCmd{Cmd: []string{"node", "server.js"}}, []string{"node", "server.js"}},
		{"imagen vieja con entrypoint", &api.ServiceSpec{Argv: []string{"x"}}, api.MachineCmd{Entrypoint: []string{"y"}}, []string{"y"}},
	}
	for _, c := range casos {
		got, err := machineArgv(c.spec, &c.c)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %q %v, want %q", c.nombre, got, err, c.want)
		}
	}
	for nombre, c := range map[string]struct {
		spec *api.ServiceSpec
		c    api.MachineCmd
	}{
		"imagen vieja":    {&api.ServiceSpec{Argv: []string{"a", "b"}}, api.MachineCmd{Cmd: []string{"c"}}},
		"init":            {&api.ServiceSpec{Argv: []string{"/init"}, Init: true}, api.MachineCmd{Cmd: []string{"c"}}},
		"nada que lanzar": {&api.ServiceSpec{Argv: []string{"a"}, Cmd: []string{"a"}}, api.MachineCmd{Entrypoint: []string{}}},
	} {
		if _, err := machineArgv(c.spec, &c.c); err == nil {
			t.Errorf("%s: want an error", nombre)
		}
	}
}

// Sin poder leer el comando, el servicio no se lanza; leído, sustituye al de
// la imagen.
func TestApplyMachineCmd(t *testing.T) {
	a := &Agent{}
	a.applyMachineCmd(false, func() (*MMDSStore, error) { t.Fatal("leyó MMDS sin kling.cmd"); return nil, nil })
	if a.machineCmd != nil || a.cmdErr != nil {
		t.Fatal("sin kling.cmd")
	}
	old := machineEnvTries
	machineEnvTries = 1
	defer func() { machineEnvTries = old }()
	a.applyMachineCmd(true, func() (*MMDSStore, error) { return nil, errors.New("no contesta") })
	if _, err := a.conMachineCmd(nil); err == nil {
		t.Fatal("want an error without the command")
	}
	b := &Agent{}
	b.applyMachineCmd(true, func() (*MMDSStore, error) {
		return &MMDSStore{MachineCmd: &api.MachineCmd{Cmd: []string{"node", "otro.js"}}}, nil
	})
	svc := newService(api.ServiceSpec{Argv: []string{"node", "server.js"}, Entrypoint: []string{}, Cmd: []string{"node", "server.js"}})
	got, err := b.conMachineCmd(svc)
	if err != nil || !slices.Equal(got.spec.Argv, []string{"node", "otro.js"}) {
		t.Fatalf("%v %v", got.spec.Argv, err)
	}
}
