//go:build !darwin

package net

// Cambio de la política de salida de una máquina VIVA (PATCH
// /machines/{ref}/egress): el modo (none, internet, allowlist) y la lista de
// dominios se reprograman sin reiniciar la microVM.
//
// Cómo, sin reconstruir la red: las reglas de FORWARD/nat que dependen del modo
// son un conjunto conocido (reglasDeModo); lo común a los tres modos
// (forwardBase, el DNAT de entrada, los topes de mangle) no se toca. El cambio
// es UN iptables-restore --noflush que borra las reglas del modo viejo que no
// están en el nuevo y añade las del nuevo que faltan: cada tabla se aplica de
// una vez, así que no hay un instante con el FORWARD abierto o cerrado de más
// entre el borrado y el alta.
//
// Qué NO cubre: las máquinas nodo de un grafo (sus reglas de enlace y
// credenciales cuelgan del modo; el llamador las rechaza) ni las que tienen
// credenciales (solo valen en allowlist).
//
// Conexiones ya abiertas: el ACCEPT de ESTABLISHED de forwardBase las dejaría
// vivir tras cerrar un destino. Si el cambio estrecha lo permitido y el host
// tiene `conntrack`, se borran las entradas que el invitado originó (el
// invitado reconecta a lo que siga permitido); sin la herramienta el cambio
// solo afecta a conexiones nuevas, y el resultado lo dice.

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ErrSinRestore: sin iptables-restore no hay cambio atómico; se prefiere no
// cambiar a dejar una ventana con la política abierta.
var ErrSinRestore = errors.New("live egress change needs iptables-restore, which is not installed")

// ResultadoEgreso dice qué hizo un cambio en caliente.
type ResultadoEgreso struct {
	// ConexionesCortadas: se borraron las conexiones que el invitado tenía
	// abiertas. false con un cambio que estrecha = solo afecta a las nuevas.
	ConexionesCortadas bool
	// Estrecha: el cambio permite menos que antes.
	Estrecha bool
}

// Variables para los tests: lo que lanza procesos.
var (
	hayConntrack  = func() bool { _, err := exec.LookPath("conntrack"); return err == nil }
	hayRestore    = func() bool { _, err := exec.LookPath("iptables-restore"); return err == nil }
	reglaPresente = func(ns string, r []string) bool {
		return exec_ok(append([]string{"ip", "netns", "exec", ns, "iptables"}, comprobacionDe(r)...))
	}
	semillasDe = resolverSemillas
)

// cambioEgreso serializa los cambios por netns.
var cambioEgreso sync.Map // ns -> *sync.Mutex

// reglasDeModo son las reglas que SOLO existen en el modo e (todas menos las de
// forwardBase y el DNAT de entrada), como las pone Setup.
func (n *Net) reglasDeModo(e Egress) [][]string {
	base := len(n.forwardBase())
	switch e {
	case EgressNone:
		return append(slices.Clone(n.egressRules(e)[base:]), n.reglasGrafo(EgressNone, false)...)
	case EgressAllowlist:
		return slices.Clone(n.reglasAllowlist()[base:])
	default:
		return slices.Clone(n.egressRules(e)[base:])
	}
}

// comoBorrado convierte una regla de alta (-A cadena ..., -I cadena pos ...) en
// su -D.
func comoBorrado(r []string) []string {
	out := make([]string, 0, len(r))
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case "-A":
			out = append(out, "-D", r[i+1])
			i++
		case "-I":
			out = append(out, "-D", r[i+1])
			i++
			if i+1 < len(r) {
				if _, err := strconv.Atoi(r[i+1]); err == nil {
					i++ // la posición no va en -D
				}
			}
		default:
			out = append(out, r[i])
		}
	}
	return out
}

func claveRegla(r []string) string { return strings.Join(r, "\x00") }

// diferenciaDeReglas da lo que hay que borrar de viejas y lo que hay que
// añadir de nuevas (por igualdad de texto: una regla común se queda donde
// está).
func diferenciaDeReglas(viejas, nuevas [][]string) (borrar, anadir [][]string) {
	enNuevas := map[string]bool{}
	for _, r := range nuevas {
		enNuevas[claveRegla(comoBorrado(r))] = true
	}
	enViejas := map[string]bool{}
	for _, r := range viejas {
		k := claveRegla(comoBorrado(r))
		enViejas[k] = true
		if !enNuevas[k] {
			borrar = append(borrar, r)
		}
	}
	cierre := claveRegla(comoBorrado(forwardCierre()))
	anadeForward := false
	for _, r := range nuevas {
		if !enViejas[claveRegla(comoBorrado(r))] {
			anadir = append(anadir, r)
			anadeForward = anadeForward || esAltaForward(r)
		}
	}
	// El DROP final de FORWARD es común a none y allowlist, pero no se puede
	// dejar donde está: lo que se añade con -A cae DETRÁS de él y nunca
	// casaría (el DNS y el ipset de la allowlist quedarían cortados). Se
	// borra y se vuelve a poner en su sitio del orden nuevo.
	if anadeForward && enViejas[cierre] && enNuevas[cierre] {
		borrar = append(borrar, forwardCierre())
		anadir = anadir[:0]
		for _, r := range nuevas {
			if k := claveRegla(comoBorrado(r)); k == cierre || !enViejas[k] {
				anadir = append(anadir, r)
			}
		}
	}
	return borrar, anadir
}

// esAltaForward dice si r añade al final de la cadena FORWARD de filter.
func esAltaForward(r []string) bool {
	return len(r) >= 2 && r[0] == "-A" && r[1] == "FORWARD"
}

// dominiosEstrechan dice si algún dominio viejo ya no está cubierto por la
// lista nueva (un sufijo cubre a sus subdominios, como en isAllowed).
func dominiosEstrechan(viejos, nuevos []string) bool {
	cubierto := func(d string, lista []string) bool {
		for _, x := range lista {
			if d == x || strings.HasSuffix(d, "."+x) {
				return true
			}
		}
		return false
	}
	nv, vj := normalizeDomains(nuevos), normalizeDomains(viejos)
	for _, d := range vj {
		if !cubierto(d, nv) {
			return true
		}
	}
	return false
}

// CambiarEgreso pasa la máquina de n del modo viejo (con sus dominios) al
// nuevo. Idempotente: se puede repetir tras un fallo a medias, partiendo de
// cualquier estado intermedio, porque solo toca lo que falta.
func (n *Net) CambiarEgreso(viejo Egress, viejosDominios []string, nuevo Egress, dominios []string) (ResultadoEgreso, error) {
	var res ResultadoEgreso
	if _, err := ParseEgress(string(nuevo)); err != nil {
		return res, err
	}
	mu, _ := cambioEgreso.LoadOrStore(n.NS, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	if !hayRestore() {
		return res, ErrSinRestore
	}
	if nuevo == EgressAllowlist {
		if len(normalizeDomains(dominios)) == 0 {
			return res, errors.New("allowlist needs at least one domain")
		}
	}
	res.Estrecha = estrecha(viejo, viejosDominios, nuevo, dominios)

	// 1. Lo que las reglas nuevas necesitan antes de existir: el ipset de la
	//    allowlist y su resolver (el de none solo contesta NXDOMAIN).
	if nuevo == EgressAllowlist {
		if viejo == EgressAllowlist {
			// Mismo modo: solo cambia la lista. Primero el resolver (deja de
			// sembrar lo retirado), luego, si estrecha, se vacía el set.
			if !fijarDominios(n.NS, dominios) {
				if err := startDNSResolver(n, dominios); err != nil {
					return res, err
				}
			}
			if res.Estrecha {
				if err := ejecutar("ip", "netns", "exec", n.NS, "ipset", "flush", n.setName()); err != nil {
					return res, err
				}
			}
			sembrarSemillas(n.NS, n.setName(), semillasDe(dominios))
			res.ConexionesCortadas = res.Estrecha && n.cortarConexiones()
			return res, nil
		}
		if err := hayIpset(); err != nil {
			return res, fmt.Errorf("allowlist mode needs ipset and it is not installed: %w", err)
		}
		if err := ejecutar("ip", "netns", "exec", n.NS, "ipset", "create", n.setName(), "hash:ip",
			"timeout", "0", "maxelem", strconv.Itoa(ipsetMaxElem), "-exist"); err != nil {
			return res, err
		}
		stopResolver(n.NS)
		if err := startDNSResolver(n, dominios); err != nil {
			return res, err
		}
	}

	// 2. El cambio de reglas, en una transacción.
	viejas := n.reglasDeModo(viejo)
	nuevas := n.reglasDeModo(nuevo)
	borrar, anadir := diferenciaDeReglas(viejas, nuevas)
	var tx [][]string
	for _, r := range borrar {
		if reglaPresente(n.NS, r) {
			tx = append(tx, comoBorrado(r))
		}
	}
	for _, r := range anadir {
		// Una regla que esta misma transacción borra (el DROP final de
		// FORWARD, que se recoloca) se añade aunque esté presente ahora.
		recolocada := slices.ContainsFunc(borrar, func(b []string) bool {
			return claveRegla(comoBorrado(b)) == claveRegla(comoBorrado(r))
		})
		if recolocada || !reglaPresente(n.NS, r) {
			tx = append(tx, r)
		}
	}
	if len(tx) > 0 {
		if err := n.restoreRules(tx); err != nil {
			// Se deja el resolver del modo nuevo: las reglas siguen siendo las
			// viejas y un reintento las cambia (la operación es idempotente).
			return res, err
		}
	}

	// 3. Lo que el modo viejo tenía y el nuevo no.
	if viejo == EgressAllowlist {
		stopResolver(n.NS)
		_ = ejecutar("ip", "netns", "exec", n.NS, "ipset", "destroy", n.setName()) // si falta, no pasa nada
	}
	if nuevo == EgressAllowlist {
		sembrarSemillas(n.NS, n.setName(), semillasDe(dominios))
	}
	if nuevo == EgressNone && viejo != EgressNone {
		stopResolver(n.NS)
		if err := n.arrancarRegistroNone(EgressNone); err != nil {
			return res, err
		}
	}
	if viejo == EgressNone && nuevo != EgressNone {
		if nuevo != EgressAllowlist {
			stopResolver(n.NS)
		}
		_ = ejecutar("ip", "netns", "exec", n.NS, "nft", "delete", "table", "ip", nftTabla)
	}
	res.ConexionesCortadas = res.Estrecha && n.cortarConexiones()
	return res, nil
}

// estrecha dice si el cambio permite menos que antes. internet < allowlist <
// none solo en lo que deja salir: pasar a una allowlist desde internet, o a
// none desde cualquiera, estrecha; entre allowlists, si retira dominios.
func estrecha(viejo Egress, viejos []string, nuevo Egress, nuevos []string) bool {
	switch {
	case viejo == nuevo && nuevo == EgressAllowlist:
		return dominiosEstrechan(viejos, nuevos)
	case viejo == nuevo:
		return false
	case nuevo == EgressNone:
		return true
	case nuevo == EgressAllowlist:
		return viejo == EgressInternet
	}
	return false
}

// cortarConexiones borra las conexiones que el invitado originó, si el host
// tiene conntrack. Las que lleguen al host (DNS, proxies) se rehacen solas.
func (n *Net) cortarConexiones() bool {
	if !hayConntrack() {
		return false
	}
	// Las del netns (lo que el invitado originó) y las del host con el NSIP
	// de origen o destino: sin ellas el siguiente paquete del remoto
	// recogería el flujo de nuevo y seguiría vivo. Las dos mitades deben ir
	// bien para prometer el corte.
	ok := conntrackBorrado(ejecutar("ip", "netns", "exec", n.NS, "conntrack", "-D", "--orig-src", GuestIP))
	return n.cortarConntrackHost() && ok
}

// cortarConntrackHost borra del conntrack del HOST las entradas con el NSIP
// de origen o de destino. Teardown lo usa también: la máquina que reutilice el
// índice no hereda flujos UDP vivos de la anterior.
func (n *Net) cortarConntrackHost() bool {
	if !hayConntrack() {
		return false
	}
	a := conntrackBorrado(ejecutar("conntrack", "-D", "--orig-dst", n.NSIP))
	b := conntrackBorrado(ejecutar("conntrack", "-D", "--orig-src", n.NSIP))
	return a && b
}

// conntrackBorrado: conntrack sale con 1 si no había nada que borrar, y eso
// tampoco es un fallo.
func conntrackBorrado(err error) bool {
	return err == nil || strings.Contains(err.Error(), "exit status 1")
}
