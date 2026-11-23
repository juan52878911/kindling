package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 80-mcp-image.sh corre como root con el entorno del daemon y entra con chroot
// en un overlay donde acaba de instalar un paquete del registro. npm y pip no
// ejecutan los scripts del paquete, pero si dejan ejecutables con el nombre que
// el paquete elija en directorios del PATH (los bin de npm en /usr/local/bin).
// Si una orden posterior dentro del chroot se resolviera por el PATH heredado,
// que suele anteponer /usr/local, ejecutaria uno de esos. Por eso toda entrada
// al chroot pasa por un unico ayudante que fija un PATH sin /usr/local, y las
// ordenes de dentro van con ruta absoluta.
//
// El guion necesita root y Linux para correr de verdad; aqui se comprueba su
// texto, que es lo que cambia cuando alguien anade un chroot nuevo.
func TestImagenMCPEntraAlChrootConPATHFijo(t *testing.T) {
	b, err := os.ReadFile("../../scripts/80-mcp-image.sh")
	if err != nil {
		t.Fatal(err)
	}
	lineas := strings.Split(string(b), "\n")

	defPath := regexp.MustCompile(`^CHROOT_PATH=(\S+)$`)
	var ruta string
	for _, l := range lineas {
		if m := defPath.FindStringSubmatch(l); m != nil {
			ruta = m[1]
		}
	}
	if ruta == "" {
		t.Fatal("el guion no fija CHROOT_PATH para las ordenes dentro del chroot")
	}
	if strings.Contains(ruta, "/usr/local") {
		t.Errorf("CHROOT_PATH=%s incluye /usr/local, donde npm deja los bin de los paquetes", ruta)
	}

	ayudante := regexp.MustCompile(`^enjaula\(\) \{ PATH="\$CHROOT_PATH" chroot "\$mnt" "\$@"; \}$`)
	chrootCrudo := regexp.MustCompile(`(^|[\s;&|(` + "`" + `])chroot\s`)
	rmCrudo := regexp.MustCompile(`(^|[\s;&|'"])(rm|env|sh)\s`)
	hayAyudante := false
	for i, l := range lineas {
		codigo := strings.TrimSpace(l)
		if strings.HasPrefix(codigo, "#") {
			continue
		}
		if ayudante.MatchString(codigo) {
			hayAyudante = true
			continue
		}
		if chrootCrudo.MatchString(codigo) {
			t.Errorf("linea %d: chroot sin pasar por enjaula: %s", i+1, codigo)
		}
		if strings.Contains(codigo, "enjaula ") && rmCrudo.MatchString(codigo) {
			t.Errorf("linea %d: orden dentro del chroot sin ruta absoluta: %s", i+1, codigo)
		}
	}
	if !hayAyudante {
		t.Error("falta el ayudante enjaula() que fija el PATH del chroot")
	}
}
