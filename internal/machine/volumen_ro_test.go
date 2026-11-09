package machine

import (
	"os"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// duenosFalsos sustituye cederVolumen por uno que apunta el dueño que
// recibiría cada volumen, para comprobar la política sin root.
func duenosFalsos(t *testing.T) map[string]int {
	t.Helper()
	duenos := map[string]int{}
	antes := cederVolumen
	cederVolumen = func(ruta string, uid, gid int) error {
		duenos[ruta] = uid
		return nil
	}
	t.Cleanup(func() { cederVolumen = antes })
	return duenos
}

// Un volumen montado ro no es del VMM (M2): lo es solo mientras una máquina
// lo tiene en escritura, y vuelve a root al soltarlo. Root es aquí el euid de
// la prueba (duenoVolumen usa os.Geteuid()).
func TestVolumenSoloDelVMMMientrasLoEscribe(t *testing.T) {
	duenos := duenosFalsos(t)
	m := conVolumen(t, "datos")
	m.priv = &Privileges{Enabled: true, UID: 4242, GID: 4243}
	ruta, root := m.volumePath("datos"), os.Geteuid()

	m.permisosVolumen(ruta)
	if duenos[ruta] != root {
		t.Fatalf("un volumen recién hecho es de %d; quería root (%d)", duenos[ruta], root)
	}

	ro := api.RunRequest{Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data", ReadOnly: true}}}
	if _, err := m.reservarVolumenes(ro, "lector", "lector"); err != nil {
		t.Fatal(err)
	}
	if duenos[ruta] != root {
		t.Fatalf("con un lector el volumen es de %d; quería root (%d)", duenos[ruta], root)
	}
	m.soltarReservas("lector")

	rw := api.RunRequest{Volume: "datos"}
	if _, err := m.reservarVolumenes(rw, "escritor", "escritor"); err != nil {
		t.Fatal(err)
	}
	if duenos[ruta] != 4242 {
		t.Fatalf("con un escritor en exclusiva el volumen es de %d; quería el VMM (4242)", duenos[ruta])
	}
	// Un arranque que no llegó a byID suelta la reserva y el disco.
	m.soltarReservas("escritor")
	if duenos[ruta] != root {
		t.Fatalf("tras soltar al escritor el volumen es de %d; quería root (%d)", duenos[ruta], root)
	}

	// Uno que sí llegó lo conserva mientras viva, y lo devuelve al pararse.
	if _, err := m.reservarVolumenes(rw, "vivo", "vivo"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.byID["vivo"] = &api.Machine{ID: "vivo", Name: "vivo", State: api.StateRunning,
		Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}}
	m.mu.Unlock()
	m.soltarReservas("vivo")
	if duenos[ruta] != 4242 {
		t.Fatalf("con su escritor vivo el volumen es de %d; quería el VMM (4242)", duenos[ruta])
	}
	m.mu.Lock()
	m.byID["vivo"].State = api.StateStopped
	m.ajustarDuenosLocked([]string{"datos"})
	m.mu.Unlock()
	if duenos[ruta] != root {
		t.Fatalf("con su escritor parado el volumen es de %d; quería root (%d)", duenos[ruta], root)
	}
}

// Un escritor que murió sin pasar por soltarReservas (el daemon con él) deja
// el disco del VMM: el siguiente lector lo recibe de root igualmente, y el
// arranque del daemon lo devuelve sin esperar a ninguno.
func TestLectorNoHeredaElVolumenDelVMM(t *testing.T) {
	duenos := duenosFalsos(t)
	m := conVolumen(t, "datos")
	m.priv = &Privileges{Enabled: true, UID: 4242, GID: 4243}
	ruta, root := m.volumePath("datos"), os.Geteuid()

	duenos[ruta] = 4242
	m.ajustarDuenosVolumenes()
	if duenos[ruta] != root {
		t.Fatalf("al arrancar el daemon, sin escritores, el volumen es de %d; quería root (%d)", duenos[ruta], root)
	}

	duenos[ruta] = 4242
	ro := api.RunRequest{Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data", ReadOnly: true}}}
	if _, err := m.reservarVolumenes(ro, "lector", "lector"); err != nil {
		t.Fatal(err)
	}
	if duenos[ruta] != root {
		t.Fatalf("el lector recibió el volumen del VMM (%d); quería root (%d)", duenos[ruta], root)
	}
}

// Una failed que se descongela de su volcado vuelve a tener sus volúmenes de
// escritura, y no si otro los tomó mientras tanto.
func TestThawDeFallidaReadmiteSusVolumenes(t *testing.T) {
	duenos := duenosFalsos(t)
	m := conVolumen(t, "datos")
	m.priv = &Privileges{Enabled: true, UID: 4242, GID: 4243}
	ruta := m.volumePath("datos")
	fallida := &api.Machine{ID: "f", Name: "f", State: api.StateFailed,
		Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}}
	m.mu.Lock()
	m.byID["f"] = fallida
	m.mu.Unlock()
	if err := m.readmitirVolumenes(fallida); err != nil {
		t.Fatal(err)
	}
	if duenos[ruta] != 4242 {
		t.Fatalf("la failed descongelada no recuperó su volumen (dueño %d)", duenos[ruta])
	}
	if _, err := m.reservarVolumenes(api.RunRequest{Volume: "datos"}, "otra", "otra"); err != nil {
		t.Fatal(err)
	}
	if err := m.readmitirVolumenes(fallida); err == nil {
		t.Fatal("se readmitió en escritura un volumen que ya escribe otra máquina")
	}
}

// Sin usuario del VMM (sin jailer, o macOS) no se toca nada.
func TestVolumenSinUsuarioDelVMMNoCambiaDeDueno(t *testing.T) {
	duenos := duenosFalsos(t)
	m := conVolumen(t, "datos")
	if _, err := m.reservarVolumenes(api.RunRequest{Volume: "datos"}, "a", "a"); err != nil {
		t.Fatal(err)
	}
	m.soltarReservas("a")
	if len(duenos) != 0 {
		t.Fatalf("sin usuario del VMM se cambió el dueño: %v", duenos)
	}
}
