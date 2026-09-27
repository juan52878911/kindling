package machine

import (
	"context"
	"os/exec"
	"os/user"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Pruebas de P7 del plan de remediación: jailer obligatorio por defecto en
// Linux, con opt-out explícito (KLING_JAILER=0) y forzado (KLING_JAILER=1).
// decidirJailer es pura -no toca el entorno, PATH ni usuarios reales-, así que
// las nueve combinaciones se prueban en tabla sin KVM, sin root y sin el
// binario de jailer instalado.

func TestDecidirJailerMacOSNuncaJailaNiAvisa(t *testing.T) {
	// jailerPosible es falso en macOS/vz: nada de esto aplica, ni siquiera con
	// KLING_JAILER=0 (que en Linux sí imprime un aviso) o =1 (que en Linux sí
	// fuerza). Es la garantía de que P7 no toca el backend vz.
	for _, forced := range []string{"", "0", "1"} {
		for _, binPresent := range []bool{false, true} {
			for _, userReady := range []bool{false, true} {
				jailed, blocked, warn := decidirJailer(false, forced, binPresent, userReady, "", false)
				if jailed || blocked != "" || warn != "" {
					t.Fatalf("decidirJailer(false, %q, %v, %v, false) = (%v, %q, %q), quería todo vacío",
						forced, binPresent, userReady, jailed, blocked, warn)
				}
			}
		}
	}
}

func TestDecidirJailerAutomaticoUsaJailerSiHayBinarioYUsuario(t *testing.T) {
	// El caso "como hoy": nada configurado, y jailer + el usuario sin
	// privilegios están listos. Se usa sin pedir nada, y no hay ni bloqueo ni
	// aviso que imprimir.
	jailed, blocked, warn := decidirJailer(true, "", true, true, "", false)
	if !jailed || blocked != "" || warn != "" {
		t.Fatalf("automático con todo listo = (%v, %q, %q), quería (true, \"\", \"\")", jailed, blocked, warn)
	}
}

func TestDecidirJailerAutomaticoSeNiegaSiFaltaAlgo(t *testing.T) {
	casos := []struct {
		nombre             string
		binPresent, userOK bool
		quiereFaltaBinario bool
		quiereFaltaUsuario bool
	}{
		{"falta el binario", false, true, true, false},
		{"falta el usuario", true, false, false, true},
		{"faltan los dos", false, false, true, true},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			jailed, blocked, warn := decidirJailer(true, "", c.binPresent, c.userOK, "", false)
			if jailed {
				t.Fatal("no debería usar jailer si falta algo y nadie forzó nada")
			}
			if warn != "" {
				t.Fatalf("el bloqueo automático no es el aviso de opt-out: %q", warn)
			}
			if blocked == "" {
				t.Fatal("automático con algo ausente debe negarse a arrancar (blocked vacío)")
			}
			if !strings.Contains(blocked, "KLING_JAILER=0") {
				t.Errorf("el error de bloqueo no explica el opt-out: %q", blocked)
			}
			if c.quiereFaltaBinario && !strings.Contains(blocked, "jailer binary isn't on PATH") {
				t.Errorf("el error no menciona el binario ausente: %q", blocked)
			}
			if c.quiereFaltaUsuario && !strings.Contains(blocked, "unprivileged user") {
				t.Errorf("el error no menciona el usuario ausente: %q", blocked)
			}
		})
	}
}

// Actualizar un host que corría como root, con jailer pero sin el usuario de
// servicio: antes de P7 se jaileaba como root, y así sigue, con un aviso que
// lleva el motivo. Sin binario no hay nada que ejecutar, aun como root.
func TestDecidirJailerAutomaticoComoRootSinUsuarioJailaYAvisa(t *testing.T) {
	jailed, blocked, warn := decidirJailer(true, "", true, false, "user kindling doesn't exist", true)
	if !jailed || blocked != "" {
		t.Fatalf("binario + root sin usuario = (%v, %q), quería jailear sin bloquear", jailed, blocked)
	}
	if !strings.Contains(warn, "ROOT") || !strings.Contains(warn, "user kindling doesn't exist") {
		t.Errorf("debería avisar de que corre como root, con el motivo: %q", warn)
	}
	if jailed, blocked, _ := decidirJailer(true, "", false, false, "", true); jailed || blocked == "" {
		t.Fatalf("sin binario, aun como root, debe bloquear: (%v, %q)", jailed, blocked)
	}
}

func TestDecidirJailerOptOutExplicitoAvisaYNoBloquea(t *testing.T) {
	// KLING_JAILER=0: nunca hay que negarse a arrancar (el usuario aceptó el
	// riesgo explícitamente), pero SIEMPRE hay que avisar fuerte al arrancar el
	// daemon, sin importar si el binario o el usuario estaban listos o no.
	for _, binPresent := range []bool{false, true} {
		for _, userReady := range []bool{false, true} {
			jailed, blocked, warn := decidirJailer(true, "0", binPresent, userReady, "", false)
			if jailed {
				t.Fatal("KLING_JAILER=0 nunca debe usar jailer")
			}
			if blocked != "" {
				t.Fatalf("KLING_JAILER=0 no debe bloquear el arranque: %q", blocked)
			}
			if warn == "" || !strings.Contains(warn, "KLING_JAILER=0") {
				t.Fatalf("KLING_JAILER=0 debe avisar y mencionarse a sí mismo: %q", warn)
			}
		}
	}
}

func TestDecidirJailerForzadoIgnoraElUsuarioPeroNoElBinario(t *testing.T) {
	// KLING_JAILER=1: se respeta aunque falte el usuario sin privilegios
	// (jailer cae a --uid/--gid 0, ver jailerArgv), pero sigue sin haber nada
	// que ejecutar si falta el propio binario.
	if jailed, blocked, warn := decidirJailer(true, "1", true, false, "", false); !jailed || blocked != "" || warn != "" {
		t.Fatalf("forzado con binario y sin usuario = (%v, %q, %q), quería (true, \"\", \"\")", jailed, blocked, warn)
	}
	jailed, blocked, warn := decidirJailer(true, "1", false, true, "", false)
	if jailed {
		t.Fatal("forzado sin binario no puede arrancar nada")
	}
	if warn != "" {
		t.Fatalf("el fallo de forzado no es el aviso de opt-out: %q", warn)
	}
	if blocked == "" || !strings.Contains(blocked, "KLING_JAILER=1") {
		t.Fatalf("el error de forzado sin binario no lo explica: %q", blocked)
	}
}

// jailerBinName y jailerBinPresent son la parte que sí toca el entorno real
// (KLING_JAILER_BIN, PATH); jailerLookPath los desacopla para no depender de
// tener jailer instalado.

func TestJailerBinNameUsaLaVariableSiEsta(t *testing.T) {
	if got := jailerBinName(); got != "jailer" {
		t.Fatalf("sin KLING_JAILER_BIN, jailerBinName() = %q, quería %q", got, "jailer")
	}
	t.Setenv("KLING_JAILER_BIN", "/opt/fc/jailer-custom")
	if got := jailerBinName(); got != "/opt/fc/jailer-custom" {
		t.Fatalf("con KLING_JAILER_BIN, jailerBinName() = %q", got)
	}
}

func TestJailerBinPresentSiguelaVariableSustituible(t *testing.T) {
	viejo := jailerLookPath
	t.Cleanup(func() { jailerLookPath = viejo })

	jailerLookPath = func(file string) (string, error) { return "", exec.ErrNotFound }
	if jailerBinPresent() {
		t.Fatal("jailerBinPresent() = true con jailerLookPath fallando")
	}

	jailerLookPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	if !jailerBinPresent() {
		t.Fatal("jailerBinPresent() = false con jailerLookPath encontrando el binario")
	}
}

// Las pruebas de cableado: que NewManager traduzca decidirJailer a los campos
// del Manager, y que boot()/runFrom()/Thaw() respeten JailerBlocked de
// verdad en vez de solo en teoría.

func TestNewManagerCableaJailerBlocked(t *testing.T) {
	if !jailerPosible {
		t.Skip("jailer solo existe en Linux")
	}
	viejo := jailerLookPath
	t.Cleanup(func() { jailerLookPath = viejo })
	jailerLookPath = func(file string) (string, error) { return "", exec.ErrNotFound }

	// runAs="" deja Privileges.Enabled en falso sin tocar /etc/passwd (ver
	// resolvePrivileges): con el binario también ausente, automático debe
	// bloquear el arranque de máquinas nuevas.
	m, err := NewManager(t.TempDir(), "firecracker", "", events.New())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(m.Close)

	if m.jailerJailed {
		t.Fatal("sin binario ni usuario, el Manager no debería marcar jailerJailed")
	}
	if m.JailerBlocked == "" {
		t.Fatal("sin binario ni usuario, JailerBlocked debería explicar por qué")
	}
	if m.JailerWarning != "" {
		t.Fatalf("el bloqueo automático no es el aviso de KLING_JAILER=0: %q", m.JailerWarning)
	}
}

func TestNewManagerCableaJailerWarningConOptOut(t *testing.T) {
	if !jailerPosible {
		t.Skip("jailer solo existe en Linux")
	}
	t.Setenv("KLING_JAILER", "0")
	m, err := NewManager(t.TempDir(), "firecracker", "", events.New())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(m.Close)

	if m.jailerJailed {
		t.Fatal("KLING_JAILER=0 no debería marcar jailerJailed")
	}
	if m.JailerBlocked != "" {
		t.Fatalf("KLING_JAILER=0 no debería bloquear el arranque: %q", m.JailerBlocked)
	}
	if m.JailerWarning == "" {
		t.Fatal("KLING_JAILER=0 debería dejar un aviso para el arranque del daemon")
	}
}

func TestBootSeNiegaSiJailerEstaBloqueado(t *testing.T) {
	// Manager armado a mano (sin pasar por NewManager, como el resto del
	// arnés): fijamos JailerBlocked directamente, que es justo lo que
	// NewManager habría calculado con el binario o el usuario ausentes.
	m := newTestManager(t)
	m.JailerBlocked = "refusing to start: jailer is required... (test)"

	pid, err := m.boot(context.Background(), "deadbeef00000001", 1, 128, 0, "", "", "", nil, nil, false)
	if pid != 0 {
		t.Fatalf("boot bloqueado devolvió pid=%d, quería 0", pid)
	}
	if err == nil || err.Error() != m.JailerBlocked {
		t.Fatalf("boot() = %v, quería exactamente el error de JailerBlocked", err)
	}
}

// Con jailer bloqueado, Run, runFrom y Thaw se niegan ANTES de publicar,
// reservar o montar nada: ni una entrada fallida en byID (contaría para
// checkMachineLimit y se acumularía con cada reintento del gateway) ni una
// reserva de snapshot colgada.
func TestJailerBloqueadoNoDejaRastro(t *testing.T) {
	m := newTestManager(t)
	m.JailerBlocked = "refusing to start: jailer is required... (test)"

	if _, err := m.Run(context.Background(), api.RunRequest{Name: "bloqueada"}); err == nil || err.Error() != m.JailerBlocked {
		t.Fatalf("Run() = %v, quería exactamente el error de JailerBlocked", err)
	}
	if _, err := m.Run(context.Background(), api.RunRequest{Name: "copia", From: "dorado"}); err == nil || err.Error() != m.JailerBlocked {
		t.Fatalf("Run(From) = %v, quería exactamente el error de JailerBlocked", err)
	}
	m.mu.RLock()
	n, res := len(m.byID), len(m.reserved)
	m.mu.RUnlock()
	if n != 0 {
		t.Fatalf("un Run bloqueado dejó %d entradas en byID, quería 0", n)
	}
	if res != 0 {
		t.Fatalf("un Run bloqueado dejó %d reservas, quería 0", res)
	}

	// Thaw de una warm: el bloqueo gana al resto de comprobaciones (aquí no
	// hay volcado, así que sin el check temprano el error sería otro).
	mc := m.addForTest("deadbeef00000002")
	m.mu.Lock()
	m.byID[mc.ID].State = api.StateWarm
	m.mu.Unlock()
	if _, err := m.Thaw(context.Background(), mc.ID); err == nil || err.Error() != m.JailerBlocked {
		t.Fatalf("Thaw() = %v, quería exactamente el error de JailerBlocked", err)
	}
	if st := vivaDe(t, m, mc.ID).State; st != api.StateWarm {
		t.Fatalf("un Thaw bloqueado dejó la máquina %s, quería warm", st)
	}
}

// El bloqueo tiene que decir la causa REAL de que no haya usuario sin
// privilegios: antes siempre decía "el usuario no existe o no tiene kvm",
// también con el daemon sin root o con -run-as vacío.
func TestJailerBloqueadoDiceLaCausaReal(t *testing.T) {
	viejoEuid, viejoGrupo := geteuid, lookupGroup
	t.Cleanup(func() { geteuid, lookupGroup = viejoEuid, viejoGrupo })

	casos := []struct {
		nombre, runAs string
		euid          int
		sinKVM        bool
		quiere        string
	}{
		{"daemon sin root", "root", 1000, false, "isn't running as root"},
		{"run-as vacío", "", 0, false, "-run-as/KLING_RUN_AS is empty"},
		{"usuario inexistente", "kindling-no-existe-xyz", 0, false, `user "kindling-no-existe-xyz" doesn't exist`},
		{"sin grupo kvm", "root", 0, true, "no kvm group"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			geteuid = func() int { return c.euid }
			lookupGroup = func(n string) (*user.Group, error) {
				if c.sinKVM {
					return nil, user.UnknownGroupError(n)
				}
				return &user.Group{Gid: "36", Name: n}, nil
			}
			priv, _ := resolvePrivileges(c.runAs)
			if priv.Enabled {
				t.Fatal("resolvePrivileges no debería habilitar el usuario en este caso")
			}
			_, blocked, _ := decidirJailer(true, "", true, priv.Enabled, priv.Motivo, false)
			if !strings.Contains(blocked, c.quiere) {
				t.Fatalf("bloqueo = %q, quería que mencionara %q", blocked, c.quiere)
			}
			if c.nombre != "usuario inexistente" && strings.Contains(blocked, "doesn't exist") {
				t.Fatalf("el bloqueo culpa a un usuario inexistente sin serlo: %q", blocked)
			}
		})
	}

	// Sin binario: lo dice, además de la causa del usuario.
	_, blocked, _ := decidirJailer(true, "", false, false, "the daemon isn't running as root", false)
	if !strings.Contains(blocked, "jailer binary isn't on PATH") || !strings.Contains(blocked, "isn't running as root") {
		t.Fatalf("bloqueo sin binario ni root = %q", blocked)
	}
}
