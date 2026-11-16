package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/internal/buildver"
)

// De qué release salieron el kernel y min.ext4 que `kling up` baja en un Mac
// (bajarImagenesMac). `kling up` solo baja los que faltan y nunca los
// sustituye: tras `kling upgrade` siguen siendo los de la release con la que
// se instaló. doctor lo dice con una marca junto a cada uno.

// marcaRelease es el fichero de images/ con la etiqueta de la release de la
// que se bajó destino (".vmlinux.release").
func marcaRelease(dir, destino string) string {
	return filepath.Join(dir, "."+destino+".release")
}

// marcarRelease apunta en dir de qué release viene destino. Un fallo no es
// grave: solo se pierde el aviso de doctor.
func marcarRelease(dir, destino, etiqueta string) {
	_ = os.WriteFile(marcaRelease(dir, destino), []byte(etiqueta+"\n"), 0o644)
}

// releaseDe lee la marca de destino en dir; "" si no hay (copiado de un host
// Linux, compilado aquí o bajado antes de que existiera la marca).
func releaseDe(dir, destino string) string {
	b, err := os.ReadFile(marcaRelease(dir, destino))
	if err != nil || len(b) > 128 {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// imagenesDeRelease es, para cada imagen que `kling up` baja en un Mac, la
// release de la que vino según su marca.
func imagenesDeRelease(dir string) map[string]string {
	out := map[string]string{}
	for _, im := range imagenesMac("arm64") {
		if r := releaseDe(dir, im.destino); r != "" {
			out[im.destino] = r
		}
	}
	return out
}

// checkImagenesRelease avisa de las imágenes bajadas de otra release que este
// kling. No es un fallo: siguen arrancando; pero el kernel no recibe los
// arreglos de la release nueva.
func checkImagenesRelease(deRelease map[string]string, cli, dir string) (doctorCheck, bool) {
	cli = strings.TrimPrefix(cli, "v")
	if buildver.IsDev(cli) {
		return doctorCheck{}, false
	}
	cli = "v" + cli
	var viejas []string
	kernel := false
	for _, im := range imagenesMac("arm64") {
		if r, ok := deRelease[im.destino]; ok && r != cli {
			viejas = append(viejas, im.destino+" from "+r)
			kernel = kernel || im.destino == "vmlinux"
		}
	}
	if len(viejas) == 0 {
		return doctorCheck{}, false
	}
	c := doctorCheck{Name: "images (release)", State: doctorWarn,
		Detail: fmt.Sprintf("this kling is %s and %s has %s: kling up only downloads them when missing", cli, dir, strings.Join(viejas, ", ")),
		Fix:    "min.ext4 is the base of your layered images: replace it only if you rebuild them"}
	if kernel {
		k := filepath.Join(dir, "vmlinux")
		c.Fix = fmt.Sprintf("for this release's kernel: stop the daemon, mv %s %s.old and run kling up (templates keep restoring; re-save one to move it to the new kernel); ", k, k) + c.Fix
	}
	return c, true
}
