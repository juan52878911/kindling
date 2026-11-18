package net

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// S2: AsegurarHost (lo que llaman el vigilante y el thaw con la red
// conservada) no hace nada si nadie pidió SetupHost, y tras un reload que
// vacía todo repone las barreras sin pasar por Setup.
func TestAsegurarHostReponeSinSetup(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	llamadas := 0
	ejecutarDelFalso := ejecutar
	ejecutar = func(args ...string) error { llamadas++; return ejecutarDelFalso(args...) }
	if err := AsegurarHost(); err != nil || llamadas != 0 {
		t.Fatalf("sin SetupHost: err %v, %d llamadas; quería nil y ninguna", err, llamadas)
	}
	if err := SetupHost(AccesoAgente{UIDs: []int{1000}}); err != nil {
		t.Fatal(err)
	}
	inputAntes := slices.Clone(f.input)
	f.input, f.otras = nil, nil // firewalld --reload
	if err := AsegurarHost(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.input, inputAntes) {
		t.Errorf("INPUT = %v, quería %v", f.input, inputAntes)
	}
	if !f.comprobar([]string{"iptables", "-C", "OUTPUT", "-d", HostSubnet, "-j", cadenaAcceso}) ||
		!f.comprobar(paraComprobar(reglaAgenteDesdeFuera())) || !f.comprobar(paraComprobar(centinelaForward())) {
		t.Errorf("faltan OUTPUT, mangle o FORWARD: %v", f.otras)
	}
}

// F3: una reposición de INPUT que falla a medias (el cerrojo de xtables
// agotado en pleno reload) no se da por buena: la siguiente deja las reglas
// enteras y en orden. Antes el centinela era la primera regla que entra, y
// con ella puesta la siguiente no reponía nada: quedaban los DROP sin el
// ACCEPT de las respuestas, y el daemon sin hablar con ningún agente.
func TestReposicionAMediasSeCompletaLaSiguiente(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	if err := SetupHost(AccesoAgente{}); err != nil {
		t.Fatal(err)
	}
	quiere := slices.Clone(f.input)
	f.input = nil
	inserciones, roto := 0, true
	f.falla = func(args []string) error {
		if roto && slices.Contains(args, "-I") && slices.Contains(args, "INPUT") {
			if inserciones++; inserciones == 3 {
				return errors.New("Another app is currently holding the xtables lock")
			}
		}
		return nil
	}
	if err := asegurarHostInput(); err == nil {
		t.Fatal("la reposición a medias = nil")
	}
	roto = false
	if err := asegurarHostInput(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.input, quiere) {
		t.Fatalf("INPUT =\n%s\nquería\n%s", strings.Join(f.input, "\n"), strings.Join(quiere, "\n"))
	}
}

// S8: el salto de OUTPUT a cadenaAcceso y la del agente en mangle solo valen
// delante. Si otro cortafuegos inserta arriba un salto a sus cadenas, se
// vuelven a poner las primeras, sin duplicarse.
func TestLasQueVanPrimerasVuelvenArriba(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	if err := SetupHost(AccesoAgente{}); err != nil {
		t.Fatal(err)
	}
	ajena := "-j cortafuegos-ajeno"
	f.otras["OUTPUT"] = slices.Insert(f.otras["OUTPUT"], 0, ajena)
	f.otras["mangle/PREROUTING"] = slices.Insert(f.otras["mangle/PREROUTING"], 0, ajena)
	if err := asegurarHostInput(); err != nil {
		t.Fatal(err)
	}
	salto := strings.Join(sinPosicion([]string{"iptables", "-I", "OUTPUT", "1", "-d", HostSubnet, "-j", cadenaAcceso}, "-C")[3:], " ")
	agente := strings.Join(sinPosicion(reglaAgenteDesdeFuera(), "-C")[5:], " ")
	for cadena, quiere := range map[string][]string{
		"OUTPUT":            {salto, ajena},
		"mangle/PREROUTING": {agente, ajena},
	} {
		if got := f.otras[cadena]; !slices.Equal(got, quiere) {
			t.Errorf("%s = %q, quería %q", cadena, got, quiere)
		}
	}
}

// `iptables -S` añade el -m tcp implícito del --dport y reordena las
// opciones: con eso la regla del agente sigue siendo la misma. Detrás de
// otra, no es la primera.
func TestEsPrimeraConLaSalidaDeIptables(t *testing.T) {
	old := listarTabla
	t.Cleanup(func() { listarTabla = old })
	var tabla, cadena string
	nuestra := "-A PREROUTING -p tcp -d " + HostSubnet + " -m tcp --dport 8080 -j DROP"
	ajena := "-A PREROUTING -j cali-PREROUTING"
	lineas := []string{"-P PREROUTING ACCEPT", nuestra, ajena}
	listarTabla = func(t, c string) ([]string, error) {
		tabla, cadena = t, c
		return lineas, nil
	}
	if ok, err := esPrimera(reglaAgenteDesdeFuera()); !ok || err != nil || tabla != "mangle" || cadena != "PREROUTING" {
		t.Fatalf("esPrimera = %v, %v (tabla %q, cadena %q)", ok, err, tabla, cadena)
	}
	lineas = []string{"-P PREROUTING ACCEPT", ajena, nuestra}
	if ok, err := esPrimera(reglaAgenteDesdeFuera()); ok || err != nil {
		t.Fatalf("detrás de otra: esPrimera = %v, %v; quería false", ok, err)
	}
}

// S9: el permiso al gid 0 sin --suppl-groups (el que quedaba con un
// iptables sin esa opción) también se quita.
func TestQuitaElPermisoViejoAlGrupoRootSinSuppl(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	f.poner(cadenaAcceso, []string{"-m owner --gid-owner 0 -j RETURN", "-m owner --gid-owner 0 --suppl-groups -j RETURN"})
	if err := setupAcceso(AccesoAgente{}, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.otras[cadenaAcceso] {
		if strings.Contains(r, "--gid-owner 0 ") {
			t.Errorf("sigue un permiso al grupo root: %s", r)
		}
	}
}

// R6: iptables-restore sin -w (ver restoreRules) reintenta mientras el
// cerrojo de xtables está tomado, y no reintenta otros fallos.
func TestRestoreReintentaConElCerrojoOcupado(t *testing.T) {
	old := ejecutarRestore
	t.Cleanup(func() { ejecutarRestore = old })
	intentos := 0
	ejecutarRestore = func(ns, entrada string) ([]byte, error) {
		if intentos++; intentos < 3 {
			return []byte("Another app is currently holding the xtables lock. Perhaps you want to use the -w option?"), errors.New("exit status 4")
		}
		return nil, nil
	}
	n := &Net{NS: "kl-prueba"}
	if err := n.restoreRules([][]string{{"iptables", "-A", "FORWARD", "-j", "DROP"}}); err != nil || intentos != 3 {
		t.Fatalf("restore = %v tras %d intentos; quería nil tras 3", err, intentos)
	}
	intentos = 0
	ejecutarRestore = func(ns, entrada string) ([]byte, error) {
		intentos++
		return []byte("iptables-restore: line 2 failed"), errors.New("exit status 1")
	}
	if err := n.restoreRules([][]string{{"iptables", "-A", "FORWARD", "-j", "DROP"}}); err == nil || intentos != 1 {
		t.Fatalf("restore con una regla mala = %v tras %d intentos; quería error tras 1", err, intentos)
	}
}

// R6: los listados de iptables que iban sin -w esperan al cerrojo
// (listadosDelNetns: los del netns, solo en Linux).
func TestListadosConEspera(t *testing.T) {
	for _, args := range append([][]string{argsLeerFiltro()}, listadosDelNetns()...) {
		i := slices.Index(args, "iptables")
		if i < 0 || i+2 >= len(args) || args[i+1] != "-w" || args[i+2] != esperaXtables {
			t.Errorf("%v sin -w %s", args, esperaXtables)
		}
	}
}
