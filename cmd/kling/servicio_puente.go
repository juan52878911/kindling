package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// EL SERVICIO DEL PUENTE (com.kindling.bridge).
//
// `kling mcp memory install-service` deja un LaunchAgent que corre kling-bridge
// por su ruta absoluta, resuelta UNA vez al instalarlo. Si el puente se mueve
// (antes lo dejaba make install en ~/.local/bin; desde que es compañero, kling
// plugin install mcp lo deja junto a kling-mcp), launchd sale con 78
// (EX_CONFIG) en cada intento y nadie lo decía. doctor lo detecta y plugin
// install / upgrade -cli lo regeneran con la ruta buena.

// etiquetaPuente es el Label del LaunchAgent y el nombre de su plist.
const etiquetaPuente = "com.kindling.bridge"

// servicioPuente es lo que se sabe del LaunchAgent del puente.
type servicioPuente struct {
	plist    string
	programa string // ProgramArguments[0] (o Program)
	existe   bool   // programa es un fichero
	// ultimaSalida es "last exit code" de `launchctl print`; "" si no se sabe
	// o si está corriendo.
	ultimaSalida string
}

// plistPuente es dónde vive el plist del servicio en home.
func plistPuente(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", etiquetaPuente+".plist")
}

// leerServicioPuente lee el plist del servicio en home; nil si no hay (o no
// se puede leer: entonces no es un servicio que instalara kindling).
func leerServicioPuente(home string) *servicioPuente {
	p := plistPuente(home)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	prog, err := programaDePlist(b)
	if err != nil || prog == "" {
		return &servicioPuente{plist: p}
	}
	fi, err := os.Stat(prog)
	return &servicioPuente{plist: p, programa: prog, existe: err == nil && !fi.IsDir()}
}

// programaDePlist saca el programa de un plist de launchd: Program, o el
// primero de ProgramArguments.
func programaDePlist(b []byte) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	var clave string // la última <key> del dict de arriba
	enArgs := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch el.Name.Local {
		case "key":
			var k string
			if err := d.DecodeElement(&k, &el); err != nil {
				return "", err
			}
			clave = strings.TrimSpace(k)
		case "array":
			enArgs = clave == "ProgramArguments"
		case "string":
			var v string
			if err := d.DecodeElement(&v, &el); err != nil {
				return "", err
			}
			if clave == "Program" || enArgs {
				return strings.TrimSpace(v), nil
			}
		}
	}
}

// reUltimaSalida saca el código de `launchctl print` ("last exit code = 78").
var reUltimaSalida = regexp.MustCompile(`last exit code = (-?\d+)`)

// ultimaSalidaPuente pregunta a launchd cómo salió el servicio; "" si no lo
// conoce o si está corriendo. Solo lee.
func ultimaSalidaPuente() string {
	out, err := exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+etiquetaPuente).Output()
	if err != nil || strings.Contains(string(out), "state = running") {
		return ""
	}
	if m := reUltimaSalida.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// sondearServicioPuente es lo que doctor mira del servicio: solo en macOS,
// donde lo instala install-service.
func sondearServicioPuente() *servicioPuente {
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	s := leerServicioPuente(home)
	if s != nil {
		s.ultimaSalida = ultimaSalidaPuente()
	}
	return s
}

// ordenReparar es cómo se regenera el plist con el puente de ahora.
func ordenReparar(mcpInstalada bool) string {
	if mcpInstalada {
		return "kling mcp memory install-service -repair"
	}
	return "kling plugin install mcp   (it repairs the service)"
}

// checkServicioPuente juzga el servicio: uno cuyo programa no existe falla en
// cada arranque (launchd: 78), así que es un fallo, no un aviso.
func checkServicioPuente(s *servicioPuente, mcpInstalada bool) (doctorCheck, bool) {
	if s == nil {
		return doctorCheck{}, false
	}
	c := doctorCheck{Name: "bridge service", State: doctorOK, Detail: etiquetaPuente + ": " + s.programa}
	switch {
	case s.programa == "":
		c.State = doctorWarn
		c.Detail = fmt.Sprintf("%s: cannot read its program from %s", etiquetaPuente, s.plist)
		c.Fix = ordenReparar(mcpInstalada)
	case !s.existe:
		c.State = doctorFail
		c.Detail = fmt.Sprintf("%s runs %s, which does not exist: launchd fails it (exit 78) on every start", etiquetaPuente, s.programa)
		c.Fix = ordenReparar(mcpInstalada)
	case s.ultimaSalida == "78":
		c.State = doctorFail
		c.Detail = fmt.Sprintf("%s: launchd cannot start %s (exit 78)", etiquetaPuente, s.programa)
		c.Fix = ordenReparar(mcpInstalada)
	case s.ultimaSalida != "" && s.ultimaSalida != "0":
		c.State = doctorWarn
		c.Detail = fmt.Sprintf("%s: %s exited with code %s", etiquetaPuente, s.programa, s.ultimaSalida)
		c.Fix = "see ~/Library/Logs/kindling-bridge.log"
	}
	return c, true
}

// repararServicioPuente regenera el servicio del puente con kling-mcp de
// extDir si el plist de home apunta a un programa que ya no existe. Devuelve
// si lo intentó. Un servicio sano, o uno sin kling-mcp con que rehacerlo, no
// se toca.
func repararServicioPuente(home, extDir string, run func(bin string, args ...string) error) (bool, error) {
	s := leerServicioPuente(home)
	if s == nil || s.programa == "" || s.existe {
		return false, nil
	}
	mcp := filepath.Join(extDir, "kling-mcp")
	if fi, err := os.Stat(mcp); err != nil || fi.IsDir() {
		return false, nil
	}
	return true, run(mcp, "memory", "install-service", "-repair")
}

// repararServicioPuenteTras es lo que plugin install mcp y upgrade -cli
// hacen al terminar: decir qué pasó, sin fallar por ello.
func repararServicioPuenteTras(extDir string) {
	if runtime.GOOS != "darwin" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	hecho, err := repararServicioPuente(home, extDir, func(bin string, args ...string) error {
		cmd := exec.Command(bin, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	})
	switch {
	case !hecho:
	case err != nil:
		fmt.Printf("warning: %s points to a missing bridge and could not be repaired: %v\n  retry with: %s\n",
			etiquetaPuente, err, ordenReparar(true))
	default:
		fmt.Printf("%s now runs the bridge installed with mcp\n", etiquetaPuente)
	}
}
