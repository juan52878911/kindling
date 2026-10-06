package guest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// kling.env es un parámetro conocido: no se avisa de él como de uno nuevo.
func TestBootParamEnv(t *testing.T) {
	p := parseBootParams("console=ttyS0 kling.env=1 kling.exec=1")
	if p.values[api.MachineEnvBootParam] != "1" || len(p.unknown) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestOverlayEnv(t *testing.T) {
	base := []string{"PATH=/bin", "PW=de-la-imagen", "HOME=/root", "PW=repetida"}
	got := overlayEnv(base, map[string]string{"PW": "de-la-maquina", "Z": "1", "A": "2"})
	want := []string{"PATH=/bin", "PW=de-la-maquina", "HOME=/root", "A=2", "Z=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("overlayEnv = %q, want %q", got, want)
	}
}

// Se reintenta lo justo; un almacén sin la clave es un fallo, no un vacío.
func TestLoadMachineEnv(t *testing.T) {
	defer func(n int, p time.Duration) { machineEnvTries, machineEnvPause = n, p }(machineEnvTries, machineEnvPause)
	machineEnvPause = 0
	machineEnvTries = 3

	n := 0
	env, err := loadMachineEnv(func() (*MMDSStore, error) {
		if n++; n < 3 {
			return nil, errors.New("sin ruta todavía")
		}
		return &MMDSStore{MachineEnv: map[string]string{"PW": "x"}}, nil
	})
	if err != nil || env["PW"] != "x" || n != 3 {
		t.Fatalf("env %v err %v intentos %d", env, err, n)
	}
	for _, st := range []*MMDSStore{nil, {Env: map[string]string{"OTRA": "y"}}} {
		if _, err := loadMachineEnv(func() (*MMDSStore, error) { return st, nil }); err == nil {
			t.Errorf("store %+v aceptado sin %s", st, api.MachineEnvMMDSKey)
		}
	}
}

// Sin kling.env no se lee nada; con él, el entorno de la máquina gana al de
// la imagen; y si no se puede leer, el servicio no arranca.
func TestApplyMachineEnvYServicio(t *testing.T) {
	defer func(n int, p time.Duration) { machineEnvTries, machineEnvPause = n, p }(machineEnvTries, machineEnvPause)
	machineEnvPause, machineEnvTries = 0, 1

	a := &Agent{Env: []string{"PATH=/usr/bin:/bin", "PW=de-la-imagen"}}
	a.applyMachineEnv(false, func() (*MMDSStore, error) { t.Fatal("leyó MMDS sin kling.env"); return nil, nil })

	a.applyMachineEnv(true, func() (*MMDSStore, error) {
		return &MMDSStore{MachineEnv: map[string]string{"PW": "de-la-maquina"}}, nil
	})
	if a.envErr != nil || !slices.Contains(a.Env, "PW=de-la-maquina") || slices.Contains(a.Env, "PW=de-la-imagen") {
		t.Fatalf("env %q err %v", a.Env, a.envErr)
	}
	s := newTestService(t, api.ServiceSpec{Argv: []string{"sh", "-c", "echo \"pw=$PW\""}, Restart: api.RestartNo})
	a.startService(s)
	waitUntil(t, "the service", func() bool { return strings.Contains(s.Status(1<<10).Log, "pw=de-la-maquina") })
	s.Stop()

	b := &Agent{Env: []string{"PATH=/usr/bin:/bin"}}
	b.applyMachineEnv(true, func() (*MMDSStore, error) { return nil, errors.New("MMDS no contesta") })
	if b.envErr == nil {
		t.Fatal("sin entorno y sin error")
	}
	s2 := newTestService(t, api.ServiceSpec{Argv: []string{"sh", "-c", "echo arrancado"}})
	b.startService(s2)
	st := s2.Status(1 << 10)
	if st.Running || st.Starts != 0 || !strings.Contains(st.Error, "MMDS no contesta") || !st.Declared {
		t.Fatalf("arrancó sin su entorno: %+v", st)
	}
	s2.Stop() // no se colgará: nunca arrancó
	if !slices.Contains(b.Caps(), api.GuestCapEnv) {
		t.Fatal("no anuncia la capacidad env")
	}
}
