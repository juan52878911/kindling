package net

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// La política del FORWARD del host.
//
// kindling solo AÑADE reglas DROP al FORWARD del host (HostEgressRules): el
// tráfico de los veth que no casa con ninguna cae en la política de la cadena,
// que en un host normal es ACCEPT. Docker la pone en DROP al arrancar, y
// entonces las máquinas con egress internet o allowlist se quedan sin salida
// en silencio: arrancan, su agente contesta (va por INPUT), y cualquier
// conexión hacia fuera muere en el host. Lo vio el e2e en el runner de CI.
//
// Aquí solo se DETECTA y se explica: cambiar las reglas que instala kindling
// es una decisión de seguridad que no se toma por el camino.

// ForwardSinSalida dice, leyendo la salida de `iptables -S` (tabla filter,
// todas las cadenas), si la política del FORWARD es DROP sin ninguna regla
// que acepte el tráfico de las máquinas: a la ida (entra por un veth vh-* con
// origen en HostSubnet) y a la vuelta (sale por un veth hacia HostSubnet).
// Una regla en cualquier cadena salvo INPUT y OUTPUT cuenta, aunque sea una a
// la que FORWARD no salte: es un aviso, y mejor callar que alarmar con un falso positivo.
func ForwardSinSalida(reglas string) bool {
	drop, ida, vuelta := false, false, false
	for _, l := range strings.Split(reglas, "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "-P" && f[1] == "FORWARD" {
			drop = f[2] == "DROP" || f[2] == "REJECT"
			continue
		}
		// INPUT y OUTPUT no reenvían: un ACCEPT de los veth en INPUT
		// (HostInputRules) no le da salida a nadie.
		if len(f) < 2 || f[0] != "-A" || f[1] == "INPUT" || f[1] == "OUTPUT" || !acepta(f) {
			continue
		}
		opts := opciones(f[2:])
		sinFiltro := len(opts) == 0
		if sinFiltro || opts["-i"] == "vh-+" || opts["-s"] == HostSubnet {
			ida = true
		}
		if sinFiltro || opts["-o"] == "vh-+" || opts["-d"] == HostSubnet {
			vuelta = true
		}
		// Un ESTABLISHED,RELATED general (sin interfaz ni red) también da la
		// vuelta: es lo que lleva un cortafuegos de host corriente.
		if opts["-i"] == "" && opts["-o"] == "" && opts["-s"] == "" && opts["-d"] == "" &&
			strings.Contains(opts["--ctstate"], "ESTABLISHED") {
			vuelta = true
		}
	}
	return drop && !(ida && vuelta)
}

// acepta dice si la regla termina en -j ACCEPT.
func acepta(f []string) bool {
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "-j" {
			return f[i+1] == "ACCEPT"
		}
	}
	return false
}

// opciones son las opciones de una regla de `iptables -S` (sin la cadena ni
// el -j), con su valor. "-m conntrack" no cuenta como filtro; "--ctstate" sí.
func opciones(f []string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case "-j":
			i++
		case "-m":
			i++
		case "!":
			// Una negación es un filtro: se guarda con su opción delante.
			if i+2 < len(f) {
				m["!"+f[i+1]] = f[i+2]
				i += 2
			}
		default:
			if !strings.HasPrefix(f[i], "-") {
				continue
			}
			op, v := f[i], ""
			if i+1 < len(f) && !strings.HasPrefix(f[i+1], "-") {
				v = f[i+1]
				i++
			}
			m[op] = v
		}
	}
	return m
}

// PorQueForward es la explicación del aviso, para `kling up -check` y el log
// del daemon.
const PorQueForward = "the host's FORWARD policy is DROP (Docker sets it) and kindling only adds DROP rules there:\n" +
	"machines with -egress internet or allowlist get no outbound traffic, silently"

// ArregloForward son las órdenes que lo arreglan sin abrir el reenvío de las
// redes de Docker (cambiar la política a ACCEPT lo abriría). Van con -A, al
// final del FORWARD, para quedar DETRÁS de las barreras de kindling (las redes
// privadas siguen cortadas): con el daemon ya arrancado, que es quien las pone.
func ArregloForward() []string {
	return []string{
		"# with the kling daemon running, so these go after its DROP rules:",
		"sudo iptables -A FORWARD -i vh-+ -s " + HostSubnet + " -j ACCEPT",
		"sudo iptables -A FORWARD -o vh-+ -d " + HostSubnet + " -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"# keep them across reboots with your distro's tool (iptables-persistent, firewalld...);",
		"# don't set the policy to ACCEPT: that also opens forwarding for Docker's networks",
		"# or let kindling keep its own chain for them: KLING_FORWARD_CHAIN=1 in the daemon's environment",
		"# (" + cadenaForward + ": its DROP rules plus those two ACCEPTs, jumped to from FORWARD)",
	}
}

// cadenaForward es la cadena propia (opcional) que lleva el FORWARD de las
// máquinas en un host con Docker: ver ReglasCadenaForward.
const cadenaForward = "KINDLING-FWD"

// CadenaForwardActiva dice si el operador pidió la cadena (KLING_FORWARD_CHAIN=1).
// Apagada por defecto: abrir el reenvío de los veth de kindling en un host con
// la política en DROP es decisión suya, no nuestra.
func CadenaForwardActiva() bool { return os.Getenv("KLING_FORWARD_CHAIN") == "1" }

// ReglasCadenaForward son las órdenes de iptables de KINDLING-FWD, en orden: las
// DROP de FORWARD de HostEgressRules (que así corren también delante de lo que
// Docker haya puesto en FORWARD) y, al final, el ACCEPT de lo que sale de las
// máquinas hacia fuera y el de la vuelta ESTABLISHED. Lo que no case, sale de
// la cadena sin veredicto y sigue por el resto de FORWARD.
func ReglasCadenaForward() [][]string {
	var reglas [][]string
	for _, r := range HostEgressRules("") {
		if len(r) >= 3 && r[1] == "-A" && r[2] == "FORWARD" {
			reglas = append(reglas, append([]string{"iptables", "-A", cadenaForward}, r[3:]...))
		}
	}
	return append(reglas,
		[]string{"iptables", "-A", cadenaForward, "-i", "vh-+", "-s", HostSubnet, "-j", "ACCEPT"},
		[]string{"iptables", "-A", cadenaForward, "-o", "vh-+", "-d", HostSubnet,
			"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"})
}

// SaltosCadenaForward son los saltos desde FORWARD a la cadena, para lo que
// entra (posición 1) y lo que sale (posición 2) por un veth de kindling.
func SaltosCadenaForward() [][]string {
	return [][]string{
		{"iptables", "-I", "FORWARD", "1", "-i", "vh-+", "-j", cadenaForward},
		{"iptables", "-I", "FORWARD", "2", "-o", "vh-+", "-j", cadenaForward},
	}
}

// montarCadenaForward crea KINDLING-FWD y los saltos si faltan. Si falta la
// última regla de la cadena (un reload la vació), la rehace entera: la cadena
// es solo nuestra. Va después de las HostEgressRules.
func montarCadenaForward() error {
	if _, err := listarTabla("filter", cadenaForward); err != nil {
		if err := ejecutar("iptables", "-N", cadenaForward); err != nil {
			return err
		}
	}
	reglas := ReglasCadenaForward()
	esta, err := comprobarEstado(paraComprobar(reglas[len(reglas)-1]))
	if err != nil {
		return err
	}
	if !esta {
		if err := ejecutar("iptables", "-F", cadenaForward); err != nil {
			return err
		}
		for _, r := range reglas {
			if err := ejecutar(r...); err != nil {
				return err
			}
		}
	}
	saltos := SaltosCadenaForward()
	if err := ponerPrimera(saltos[0]); err != nil {
		return err
	}
	if esta, err := comprobarEstado(paraComprobar(saltos[1])); err != nil || esta {
		return err
	}
	return ejecutar(saltos[1]...)
}

// centinelaCadenaForward es el salto de entrada, que debe ser el primero de
// FORWARD, si la cadena está pedida; nil si no.
func centinelaCadenaForward() []string {
	if !CadenaForwardActiva() {
		return nil
	}
	return SaltosCadenaForward()[0]
}

// AvisoForward es el aviso entero (explicación y arreglo) si reglas, la
// salida de `iptables -S`, tiene el problema; "" si no.
func AvisoForward(reglas string) string {
	if !ForwardSinSalida(reglas) {
		return ""
	}
	return PorQueForward + ". Fix:\n  " + strings.Join(ArregloForward(), "\n  ")
}

// LeerFiltro devuelve `iptables -S` (tabla filter). Hace falta root.
func LeerFiltro() (string, error) {
	c := argsLeerFiltro()
	out, err := exec.Command(c[0], c[1:]...).Output()
	return string(out), err
}

// argsLeerFiltro es el iptables de LeerFiltro, con -w (conEspera): sin él,
// con iptables-legacy y un arranque a la vez, fallaba y el aviso no salía.
func argsLeerFiltro() []string { return conEspera([]string{"iptables", "-S"}) }

// FaltaParaAllowlist dice, si el host no puede ofrecer el modo allowlist, por
// qué (qué paquete falta); "" si puede, o si el sistema no usa ipset (macOS).
// Available no lo exige a propósito: sin ipset el daemon sirve none e
// internet, y solo falla quien pide allowlist, tarde y con un error suelto.
func FaltaParaAllowlist() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if err := hayIpset(); err != nil {
		return "ipset is not installed: -egress allowlist will fail (apt install ipset conntrack)"
	}
	return ""
}
