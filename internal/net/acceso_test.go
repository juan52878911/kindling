package net

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// conexionHost es una conexión que un proceso del host abre hacia HostSubnet,
// tal como la ve la cadena cadenaAcceso.
type conexionHost struct {
	uid    int
	gids   []int // el primario y los suplementarios
	dport  int
	local  bool   // el destino es una dirección del host (el lado host de un veth)
	estado string // NEW, ESTABLISHED...
}

// veredictoAcceso recorre los permisos y el cierre de reglasAcceso en el
// orden en que quedan en la cadena (los permisos con -I 1, delante) y
// devuelve el target de la primera que casa, o RETURN si no casa ninguna.
// Entiende solo lo que usa reglasAcceso: lo demás tumba el test.
func veredictoAcceso(t *testing.T, permisos, cierre [][]string, c conexionHost) string {
	t.Helper()
	cadena := append(slices.Clone(permisos), cierre...)
	for _, r := range cadena {
		objetivo, casa := "", true
		for i := 0; i < len(r) && casa; i++ {
			switch r[i] {
			case "iptables", "-m", "1", cadenaAcceso:
			case "-I", "-A":
			case "--uid-owner":
				u, _ := strconv.Atoi(r[i+1])
				casa = c.uid == u
				i++
			case "--gid-owner":
				g, _ := strconv.Atoi(r[i+1])
				casa = slices.Contains(c.gids, g)
				i++
			case "--suppl-groups":
			case "-p":
				casa = r[i+1] == "tcp"
				i++
			case "--dport":
				casa = r[i+1] == strconv.Itoa(c.dport)
				i++
			case "--dst-type":
				casa = r[i+1] == "LOCAL" && c.local
				i++
			case "--ctstate":
				casa = r[i+1] == c.estado
				i++
			case "-j":
				objetivo = r[i+1]
				i++
			case "--reject-with":
				i++
			case "owner", "addrtype", "conntrack":
			default:
				t.Fatalf("veredictoAcceso no entiende %q en %v", r[i], r)
			}
		}
		if casa && objetivo != "" {
			return objetivo
		}
	}
	return "RETURN"
}

// S1: desde el host solo llegan al agente (8080) y a los lados host de los
// veth root, el daemon y quien puede usar su socket. Cualquier otro usuario
// sigue llegando a los puertos de servicio de cada máquina (kling db,
// kling.ports), y nadie pierde las respuestas de lo que ya está abierto.
func TestAccesoAlAgenteSoloDelDaemonYDelDuenoDelSocket(t *testing.T) {
	permisos, cierre, salto := reglasAcceso(AccesoAgente{UIDs: []int{1000}, GIDs: []int{1000}}, 0, 0)
	if got := strings.Join(salto, " "); got != "iptables -I OUTPUT 1 -d "+HostSubnet+" -j "+cadenaAcceso {
		t.Fatalf("salto = %q", got)
	}
	nadie := []int{65534}
	for _, c := range []struct {
		que  string
		con  conexionHost
		want string
	}{
		{"otro usuario al agente", conexionHost{uid: 65534, gids: nadie, dport: 8080, estado: "NEW"}, "REJECT"},
		{"otro usuario a un proxy de arista", conexionHost{uid: 65534, gids: nadie, dport: 5400, local: true, estado: "NEW"}, "REJECT"},
		{"otro usuario al resolver", conexionHost{uid: 65534, gids: nadie, dport: 5333, local: true, estado: "NEW"}, "REJECT"},
		{"otro usuario al Postgres de una máquina", conexionHost{uid: 65534, gids: nadie, dport: 5432, estado: "NEW"}, "RETURN"},
		{"respuesta de un servicio del host", conexionHost{uid: 65534, gids: nadie, dport: 8080, local: true, estado: "ESTABLISHED"}, "RETURN"},
		{"root", conexionHost{uid: 0, gids: []int{0}, dport: 8080, estado: "NEW"}, "RETURN"},
		{"el dueño del socket", conexionHost{uid: 1000, gids: []int{1000}, dport: 8080, estado: "NEW"}, "RETURN"},
		{"alguien de su grupo", conexionHost{uid: 1001, gids: []int{1001, 1000}, dport: 5400, local: true, estado: "NEW"}, "RETURN"},
	} {
		if got := veredictoAcceso(t, permisos, cierre, c.con); got != c.want {
			t.Errorf("%s: %s, quería %s", c.que, got, c.want)
		}
	}
	// Sin socket cedido, el daemon (euid y egid) y root.
	permisos, cierre, _ = reglasAcceso(AccesoAgente{}, 0, 0)
	if got := veredictoAcceso(t, permisos, cierre, conexionHost{uid: 1000, gids: []int{1000}, dport: 8080, estado: "NEW"}); got != "REJECT" {
		t.Errorf("sin socket cedido, el uid 1000 al agente: %s, quería REJECT", got)
	}
}

// Dos daemons en el mismo host (el de systemd y uno privado) comparten la
// cadena: el segundo añade su permiso delante de los REJECT sin quitar el del
// primero, y volver a montar no duplica nada.
func TestSetupAccesoDeDosDaemonsNoSePisan(t *testing.T) {
	f := &iptablesFalso{}
	oldC, oldE := comprobarEstado, ejecutar
	t.Cleanup(func() { comprobarEstado, ejecutar = oldC, oldE })
	comprobarEstado, ejecutar = f.estado, f.ejecutar

	if err := setupAcceso(AccesoAgente{UIDs: []int{1000}, GIDs: []int{1000}}, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := setupAcceso(AccesoAgente{UIDs: []int{1001}, GIDs: []int{1001}}, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := setupAcceso(AccesoAgente{UIDs: []int{1000}, GIDs: []int{1000}}, 0, 0); err != nil {
		t.Fatal(err)
	}
	cadena := f.otras[cadenaAcceso]
	for _, u := range []string{"--uid-owner 0 ", "--uid-owner 1000 ", "--uid-owner 1001 ", "--gid-owner 1000 ", "--gid-owner 1001 "} {
		n := 0
		for _, r := range cadena {
			if strings.Contains(r, u) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q aparece %d veces en %v", u, n, cadena)
		}
	}
	// Los REJECT al final, una vez cada uno.
	if len(cadena) != 7 || !strings.Contains(cadena[5], "--dport 8080") || !strings.Contains(cadena[6], "--dst-type LOCAL") {
		t.Errorf("%s =\n%s", cadenaAcceso, strings.Join(cadena, "\n"))
	}
	if salto := f.otras["OUTPUT"]; len(salto) != 1 || salto[0] != "-d "+HostSubnet+" -j "+cadenaAcceso {
		t.Errorf("OUTPUT = %v", salto)
	}
}

func TestPuertoAgenteEsElDelInvitado(t *testing.T) {
	if puertoAgente != api.GuestPort {
		t.Fatalf("puertoAgente = %d, api.GuestPort = %d", puertoAgente, api.GuestPort)
	}
}

// reenviado es lo que el host hace con un paquete que reenvía: mangle
// PREROUTING (lo de -I 1 delante) y, si pasa, FORWARD con delante (lo que el
// host ya tenía arriba, como los ACCEPT de Docker) y detrás lo nuestro.
func reenviado(t *testing.T, delante, reglas [][]string, p paquete) string {
	t.Helper()
	var mangle, forward [][]string
	for _, r := range reglas {
		l := strings.Join(r, " ")
		switch {
		case strings.Contains(l, " -t mangle ") && strings.Contains(l, " PREROUTING "):
			c := slices.Clone(r)
			c = slices.Delete(c, slices.Index(c, "-t"), slices.Index(c, "-t")+2)
			c[slices.Index(c, "PREROUTING")] = "FORWARD"
			if slices.Contains(c, "-I") {
				mangle = append([][]string{c}, mangle...)
			} else {
				mangle = append(mangle, c)
			}
		case !strings.Contains(l, " -t ") && strings.Contains(l, " FORWARD "):
			forward = append(forward, r)
		}
	}
	if veredicto(t, mangle, p) == "DROP" {
		return "DROP"
	}
	return veredicto(t, append(slices.Clone(delante), forward...), p)
}

// S1: desde fuera del host (la LAN, un contenedor con red puente, otra
// máquina) no se abre conexión al agente de ninguna máquina, aunque Docker
// haya puesto sus ACCEPT de FORWARD delante de los nuestros (lo que hace cada
// vez que arranca); a los demás puertos, como antes.
func TestHostEgressRulesCierranElAgenteDesdeFuera(t *testing.T) {
	n := Plan(1, "abcdef0123")
	// La IP de la máquina en el host (n.NSIP en Linux; en macOS NSIP es GuestIP).
	maq := hostPrefix + ".0.6"
	docker := [][]string{{"iptables", "-I", "FORWARD", "1", "-i", "docker0", "!", "-o", "docker0", "-j", "ACCEPT"}}
	for _, c := range []struct {
		que  string
		p    paquete
		want string
	}{
		{"de la LAN al agente", paquete{in: "eth0", out: n.HostIf, src: "192.168.1.20", dst: maq, proto: "tcp", dport: 8080, estado: "NEW"}, "DROP"},
		{"de un contenedor al agente", paquete{in: "docker0", out: n.HostIf, src: "172.17.0.2", dst: maq, proto: "tcp", dport: 8080, estado: "NEW"}, "DROP"},
		{"de otra máquina al agente", paquete{in: "vh-otra", out: n.HostIf, src: hostPrefix + ".0.10", dst: maq, proto: "tcp", dport: 8080, estado: "NEW"}, "DROP"},
		{"de la LAN al Postgres de una máquina", paquete{in: "eth0", out: n.HostIf, src: "192.168.1.20", dst: maq, proto: "tcp", dport: 5432, estado: "NEW"}, "ACCEPT"},
		{"de un contenedor al Postgres de una máquina", paquete{in: "docker0", out: n.HostIf, src: "172.17.0.2", dst: maq, proto: "tcp", dport: 5432, estado: "NEW"}, "ACCEPT"},
	} {
		if got := reenviado(t, docker, HostEgressRules(""), c.p); got != c.want {
			t.Errorf("%s (con Docker delante): %s, quería %s", c.que, got, c.want)
		}
	}
}

// S2: un iptables sin --suppl-groups (anterior a la 1.8.3, o sin xt_owner
// completo) no deja el host sin la cadena: SetupHost la monta con el grupo
// primario, con sus REJECT y su salto.
func TestSetupHostSinSupplGroupsCierraIgual(t *testing.T) {
	f := &iptablesFalso{falla: func(args []string) error {
		if slices.Contains(args, "--suppl-groups") {
			return errors.New("unknown option \"--suppl-groups\"")
		}
		return nil
	}}
	usarIptablesFalso(t, f)
	if err := SetupHost(AccesoAgente{UIDs: []int{1000}, GIDs: []int{1000}}); err != nil {
		t.Fatalf("SetupHost = %v", err)
	}
	cadena := strings.Join(f.otras[cadenaAcceso], "\n")
	for _, quiere := range []string{"--gid-owner 1000 -j RETURN", "--dport 8080 -m conntrack --ctstate NEW -j REJECT", "--dst-type LOCAL -m conntrack --ctstate NEW -j REJECT"} {
		if !strings.Contains(cadena, quiere) {
			t.Errorf("falta %q en %s:\n%s", quiere, cadenaAcceso, cadena)
		}
	}
	if !f.comprobar([]string{"iptables", "-C", "OUTPUT", "-d", HostSubnet, "-j", cadenaAcceso}) {
		t.Errorf("sin salto a %s: OUTPUT = %v", cadenaAcceso, f.otras["OUTPUT"])
	}
	if !inputMontado.Load() {
		t.Error("SetupHost fue bien y no lo apunta")
	}
}

// S2: si SetupHost no consigue montar las barreras, ninguna máquina recibe red
// (asegurarHostInput, lo primero de Setup, falla) hasta que un reintento las
// monta. Antes el daemon seguía con un WARNING y Setup no comprobaba nada.
func TestSinBarrerasDelHostNoHayRed(t *testing.T) {
	roto := true
	f := &iptablesFalso{falla: func(args []string) error {
		if roto && slices.Contains(args, "-N") {
			return errors.New("can't initialize iptables table `filter'")
		}
		return nil
	}}
	usarIptablesFalso(t, f)
	if err := asegurarHostInput(); !errors.Is(err, ErrHostSinBarreras) {
		t.Fatalf("sin SetupHost: asegurarHostInput = %v, quería ErrHostSinBarreras", err)
	}
	if err := SetupHost(AccesoAgente{UIDs: []int{1000}, GIDs: []int{1000}}); err == nil {
		t.Fatal("SetupHost = nil con iptables -N fallando")
	}
	if err := asegurarHostInput(); !errors.Is(err, ErrHostSinBarreras) {
		t.Fatalf("tras un SetupHost fallido: asegurarHostInput = %v, quería ErrHostSinBarreras", err)
	}
	// Lo que montó antes de fallar sigue cerrando: INPUT entero.
	if len(f.input) != len(HostInputRules()) {
		t.Errorf("INPUT = %v", f.input)
	}
	roto = false
	if err := asegurarHostInput(); err != nil {
		t.Fatalf("con iptables ya bien, el reintento = %v", err)
	}
	if !f.comprobar([]string{"iptables", "-C", "OUTPUT", "-d", HostSubnet, "-j", cadenaAcceso}) {
		t.Error("el reintento no montó el salto a la cadena")
	}
}

// S15: con el daemon como root (egid 0), un usuario del grupo root que no es
// root no llega al agente; y el permiso al gid 0 de versiones anteriores se
// quita al arrancar.
func TestAccesoSinPermisoAlGrupoRoot(t *testing.T) {
	permisos, cierre, _ := reglasAcceso(AccesoAgente{}, 0, 0)
	delGrupoRoot := conexionHost{uid: 1001, gids: []int{1001, 0}, dport: 8080, estado: "NEW"}
	if got := veredictoAcceso(t, permisos, cierre, delGrupoRoot); got != "REJECT" {
		t.Errorf("un usuario del grupo root al agente: %s, quería REJECT", got)
	}
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	f.poner(cadenaAcceso, []string{"-m owner --gid-owner 0 --suppl-groups -j RETURN", "-p tcp --dport 8080 -m conntrack --ctstate NEW -j REJECT"})
	if err := setupAcceso(AccesoAgente{}, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.otras[cadenaAcceso] {
		if strings.Contains(r, "--gid-owner 0 ") {
			t.Errorf("sigue el permiso al grupo root: %s", r)
		}
	}
}

// S15: la cadena ya no solo crece. Si el socket de un daemon pasa a otro
// usuario, el permiso del anterior se va al volver a montar; los de otro
// daemon del mismo host (otra etiqueta) no se tocan.
func TestSetupAccesoQuitaLosPermisosQueYaNoDa(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	otro := AccesoAgente{UIDs: []int{1001}, GIDs: []int{1001}, Dueno: "/srv/otro"}
	if err := setupAcceso(otro, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := setupAcceso(AccesoAgente{UIDs: []int{1000}, GIDs: []int{1000}, Dueno: "/var/lib/kindling"}, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := setupAcceso(AccesoAgente{UIDs: []int{1002}, GIDs: []int{1002}, Dueno: "/var/lib/kindling"}, 0, 0); err != nil {
		t.Fatal(err)
	}
	cadena := strings.Join(f.otras[cadenaAcceso], "\n")
	for _, ya := range []string{"--uid-owner 1000 ", "--gid-owner 1000 "} {
		if strings.Contains(cadena, ya) {
			t.Errorf("sigue %q del dueño anterior:\n%s", ya, cadena)
		}
	}
	for _, sigue := range []string{"--uid-owner 1002 ", "--gid-owner 1002 ", "--uid-owner 1001 ", "--gid-owner 1001 "} {
		if !strings.Contains(cadena, sigue) {
			t.Errorf("falta %q:\n%s", sigue, cadena)
		}
	}
	if n := strings.Count(cadena, "-j REJECT"); n != 2 {
		t.Errorf("%d REJECT, quería 2:\n%s", n, cadena)
	}
}
