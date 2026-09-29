// kling-phoned: el agente del teléfono Android dentro de la microVM.
//
// Sustituye a image/android-launch.sh y a image/android-sh (docs/phoned.md):
//
//   - arranca Android (Redroid) como lo hacía el lanzador: espacios de nombres,
//     bind + pivot_root, /data (overlay o ext4 en tmpfs), la red veth por
//     netlink, y lo relanza si muere;
//   - sirve la API del teléfono en un puerto del invitado (8091 por defecto),
//     para usarla por POST /machines/{ref}/guest sin allow_exec;
//   - reenvía adb (5555) de la VM al espacio de red de Android sin DNAT
//     (ANDROID_PORTS para otros: el VNC de Redroid ya no va por defecto, #96);
//   - hace de sonda de listo (/etc/kindling/ready → `kling-phoned ready`) y de
//     gancho tras restaurar (/etc/kindling/post-restore.d/10-identity →
//     `kling-phoned identity`), con la identidad por clon de MMDS.
//
// Un solo binario estático; el SERVICE de la imagen lo arranca sin argumentos.
package main

import (
	"fmt"
	"os"
)

// version la pone el enlazador (-ldflags "-X main.version=...").
var version = "dev"

// stage2Arg es el subcomando interno del hijo que acaba siendo init.
const stage2Arg = "stage2"

const usage = `usage: kling-phoned [run]      launch Android and serve the phone API (the image SERVICE)
       kling-phoned ready          exit 0 when Android finished booting (/etc/kindling/ready)
       kling-phoned identity       apply this clone's identity from MMDS (post-restore hook)
       kling-phoned version
`

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "run":
		err = runDaemon()
	case stage2Arg:
		err = runStage2()
	case "ready":
		os.Exit(runReady())
	case "identity":
		os.Exit(runIdentity())
	case "version", "-version", "--version":
		fmt.Println("kling-phoned", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kling-phoned:", err)
		os.Exit(1)
	}
}
