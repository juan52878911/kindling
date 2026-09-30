package main

import (
	"bufio"
	"os"
	"strings"
)

// Lo de la traducción ARM que enseña /v1/health (docs/traduccion-arm.md).

// splitABIs parte ro.product.cpu.abilist ("x86_64,arm64-v8a").
func splitABIs(v string) []string {
	var out []string
	for _, a := range strings.Split(v, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// bridgeName es el puente nativo, o "" si no hay ("0" es "ninguno").
func bridgeName(v string) string {
	if v = strings.TrimSpace(v); v == "0" {
		return ""
	}
	return v
}

// binfmtState lee la primera línea de un registro de binfmt_misc
// ("enabled"/"disabled"); "" si no existe.
func binfmtState(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	l, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(l)
}

// imageValue lee una clave de IMAGE.txt (clave=valor por línea, lo escribe el
// constructor). "" si no está: una imagen anterior no lo dice.
func imageValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
