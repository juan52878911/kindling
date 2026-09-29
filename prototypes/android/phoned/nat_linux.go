package main

// Las tres reglas de netfilter de la red veth, con iptables-legacy.
//
// Por qué no netlink: el kernel del prototipo no trae nf_tables (config-android
// habilita solo xtables, que es lo que usa netd) y está fijado por sha256, y
// xtables no se programa por netlink sino con setsockopt(IPT_SO_SET_REPLACE),
// que sustituye la tabla ENTERA con un blob binario dependiente de la versión
// de cada match y target. Reimplementar eso es mantener un iptables pequeño
// para tres reglas que se ponen una vez por arranque de Android. El resto de
// la red (espacio de red, veth, direcciones, rutas, reenvío de puertos) no usa
// ningún binario.
//
// Qué queda de netfilter:
//   - MASQUERADE de lo que sale de Android por el eth0 de la VM: sale con la IP
//     de la VM y la política de egress de kindling sigue mandando;
//   - DROP de todo lo que entra a la VM desde el enlace de Android: Android
//     no puede hablar con kling-guest (:8080), con esta API (:8091) ni con
//     nada que escuche en la VM;
//   - DROP de lo que Android mande a 169.254.0.0/16 (MMDS: los secretos de la
//     VM no son del teléfono).
// El DNAT de adb de antes ya no existe: lo hace el reenvío de launch_linux.go
// desde dentro del espacio de red de Android.

import (
	"fmt"
	"os/exec"
	"strings"
)

func iptablesPath() string {
	for _, c := range []string{"iptables-legacy", "iptables"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// natSetup deja las cadenas propias vacías y rellenas; idempotente (Android se
// relanza y el lanzador vuelve a pasar por aquí).
func natSetup(ipt, hostIf, androidIP string) error {
	run := func(args ...string) error {
		out, err := exec.Command(ipt, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iptables %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	quiet := func(args ...string) bool { return exec.Command(ipt, args...).Run() == nil }
	chains := []struct{ table, chain, parent string }{
		{"nat", "KANDROID_POST", "POSTROUTING"},
		{"filter", "KANDROID_IN", "INPUT"},
		{"filter", "KANDROID_FWD", "FORWARD"},
	}
	for _, c := range chains {
		if !quiet("-t", c.table, "-N", c.chain) {
			if err := run("-t", c.table, "-F", c.chain); err != nil {
				return err
			}
		}
		if !quiet("-t", c.table, "-C", c.parent, "-j", c.chain) {
			op := "-I"
			if c.table == "nat" {
				op = "-A"
			}
			if err := run("-t", c.table, op, c.parent, "-j", c.chain); err != nil {
				return err
			}
		}
	}
	// El DNAT del lanzador de bash, si la VM vino de ahí (no pasa en una imagen
	// nueva, pero un dorado viejo restaurado con el binario nuevo sí).
	if quiet("-t", "nat", "-F", "KANDROID_PRE") {
		_ = run("-t", "nat", "-D", "PREROUTING", "-j", "KANDROID_PRE")
	}
	for _, r := range [][]string{
		{"-t", "nat", "-A", "KANDROID_POST", "-s", androidIP + "/32", "-o", "eth0", "-j", "MASQUERADE"},
		{"-A", "KANDROID_IN", "-i", hostIf, "-j", "DROP"},
		{"-A", "KANDROID_FWD", "-i", hostIf, "-d", "169.254.0.0/16", "-j", "DROP"},
	} {
		if err := run(r...); err != nil {
			return err
		}
	}
	return nil
}
