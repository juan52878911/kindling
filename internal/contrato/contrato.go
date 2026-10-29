// Package contrato compara las superficies públicas de kindling (rutas del
// daemon, códigos de estado, etiquetas JSON de pkg/api, salida -json del CLI)
// con sus ficheros golden de testdata, y aplica la regla de api.APIVersion:
//
//   - una línea que DESAPARECE o CAMBIA es un cambio incompatible: el test
//     falla, y -update se niega, mientras api.APIVersion no suba por encima de
//     la que apunta el golden ("# api: N");
//   - una línea NUEVA es compatible, pero hay que regenerar con -update para
//     dejar constancia de que se miró;
//   - subir APIVersion obliga a regenerar todos los goldens (la cabecera cambia).
//
// Solo lo usan los tests; vive fuera de _test.go para que lo compartan
// internal/daemon, pkg/api y cmd/kling.
package contrato

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// cabeceraAPI es la línea que apunta el API con el que se generó el golden.
const cabeceraAPI = "# api: "

// Leer devuelve las líneas de datos de un golden (sin comentarios ni vacías)
// y el API que apunta (0 si no lo apunta).
func Leer(ruta string) (lineas []string, api int, err error) {
	b, err := os.ReadFile(ruta)
	if err != nil {
		return nil, 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, cabeceraAPI):
			api, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(l, cabeceraAPI)))
		case strings.TrimSpace(l) == "", strings.HasPrefix(l, "#"):
		default:
			lineas = append(lineas, l)
		}
	}
	return lineas, api, sc.Err()
}

// Comparar compara las líneas de ahora con el golden de ruta. cabecera son
// las líneas de comentario que encabezan el fichero (sin "# "), que explican
// qué es y cómo se regenera. Devuelve nil si coinciden (o si actualizar lo
// reescribió); un error con lo que cambió si no.
func Comparar(ruta string, apiVersion int, cabecera []string, ahora []string, actualizar bool) error {
	antes, apiGolden, err := Leer(ruta)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	existe := err == nil
	quitadas, nuevas := diferencia(antes, ahora), diferencia(ahora, antes)

	if existe && len(quitadas) > 0 && apiVersion <= apiGolden {
		return fmt.Errorf("%s: incompatible change (%d line(s) removed or changed) without bumping api.APIVersion (still %d, pkg/api/apiversion.go).\n"+
			"Restore them, or bump APIVersion and regenerate with -update:\n%s%s",
			ruta, len(quitadas), apiVersion, listar("- ", quitadas), listar("+ ", nuevas))
	}
	if existe && len(quitadas) == 0 && len(nuevas) == 0 && apiGolden == apiVersion {
		return nil
	}
	if !actualizar {
		motivo := "new lines (compatible)"
		switch {
		case !existe:
			motivo = "the golden file does not exist"
		case apiGolden != apiVersion && len(quitadas) == 0 && len(nuevas) == 0:
			motivo = fmt.Sprintf("api.APIVersion is %d and the golden says %d", apiVersion, apiGolden)
		case len(quitadas) > 0:
			motivo = fmt.Sprintf("api.APIVersion went from %d to %d", apiGolden, apiVersion)
		}
		return fmt.Errorf("%s is out of date: %s; regenerate it with go test -update and review the diff:\n%s%s",
			ruta, motivo, listar("- ", quitadas), listar("+ ", nuevas))
	}
	return Escribir(ruta, apiVersion, cabecera, ahora)
}

// Escribir deja el golden con su cabecera, el API y las líneas ordenadas.
func Escribir(ruta string, apiVersion int, cabecera []string, lineas []string) error {
	var b strings.Builder
	for _, c := range cabecera {
		b.WriteString(strings.TrimRight("# "+c, " ") + "\n")
	}
	fmt.Fprintf(&b, "%s%d\n\n", cabeceraAPI, apiVersion)
	ord := append([]string(nil), lineas...)
	sort.Strings(ord)
	for _, l := range ord {
		b.WriteString(l + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(ruta), 0o755); err != nil {
		return err
	}
	return os.WriteFile(ruta, []byte(b.String()), 0o644)
}

// diferencia son las líneas de a que no están en b.
func diferencia(a, b []string) []string {
	en := make(map[string]bool, len(b))
	for _, l := range b {
		en[l] = true
	}
	var out []string
	for _, l := range a {
		if !en[l] {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func listar(prefijo string, ls []string) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString("  " + prefijo + l + "\n")
	}
	return b.String()
}
