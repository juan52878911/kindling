package net

import (
	"os/exec"
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
	}
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
	out, err := exec.Command("iptables", "-S").Output()
	return string(out), err
}
