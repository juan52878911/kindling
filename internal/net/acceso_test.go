package net

import (
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
	oldC, oldE := comprobarRegla, ejecutar
	t.Cleanup(func() { comprobarRegla, ejecutar = oldC, oldE })
	comprobarRegla, ejecutar = f.comprobar, f.ejecutar

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
	for _, u := range []string{"--uid-owner 0 ", "--uid-owner 1000 ", "--uid-owner 1001 ", "--gid-owner 0 ", "--gid-owner 1000 ", "--gid-owner 1001 "} {
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
	if len(cadena) != 8 || !strings.Contains(cadena[6], "--dport 8080") || !strings.Contains(cadena[7], "--dst-type LOCAL") {
		t.Errorf("%s =\n%s", cadenaAcceso, strings.Join(cadena, "\n"))
	}
	if salto := f.otras["OUTPUT"]; len(salto) != 1 || salto[0] != "-d "+HostSubnet+" -j "+cadenaAcceso {
		t.Errorf("OUTPUT = %v", salto)
	}
}

// Desde fuera del host (la LAN, un contenedor con red puente) no se abre
// conexión al agente de ninguna máquina; a los demás puertos, como antes.
func TestHostEgressRulesCierranElAgenteDesdeFuera(t *testing.T) {
	n := Plan(1, "abcdef0123")
	desdeLAN := paquete{in: "eth0", out: n.HostIf, src: "192.168.1.20", dst: n.NSIP, proto: "tcp", dport: 8080, estado: "NEW"}
	if got := veredicto(t, HostEgressRules(""), desdeLAN); got != "DROP" {
		t.Errorf("de la LAN al agente: %s, quería DROP", got)
	}
	desdeLAN.dport = 5432
	if got := veredicto(t, HostEgressRules(""), desdeLAN); got != "ACCEPT" {
		t.Errorf("de la LAN al Postgres de una máquina: %s, quería ACCEPT (como antes)", got)
	}
}

func TestPuertoAgenteEsElDelInvitado(t *testing.T) {
	if puertoAgente != api.GuestPort {
		t.Fatalf("puertoAgente = %d, api.GuestPort = %d", puertoAgente, api.GuestPort)
	}
}
