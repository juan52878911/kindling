package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// waitGuest espera a que el servidor de dentro de la microVM abra su puerto.
// La espera ocurre en el daemon: las IP de los invitados no son alcanzables
// desde un CLI remoto, y sondearlas desde aquí falla siempre por SSH.
func waitGuest(ctx context.Context, c *api.Client, ref string, timeout time.Duration) error {
	_, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: 8080, WaitMS: int(timeout / time.Millisecond), ProbeOnly: true,
	})
	return err
}

// splitDomains parte una lista de dominios separada por comas, recortando
// espacios y descartando vacíos. Acepta también espacios como separador para
// tolerar "dom1, dom2".
func splitDomains(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if d := strings.TrimSpace(f); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// ── kling version ─────────────────────────────────────────────────────────────

// cmdVersion imprime la versión del CLI y, si contesta en un segundo, la del
// daemon. El desfase entre los dos se descubría por un 404 en un comando
// nuevo; así se ve antes. Un daemon que no contesta no es un error aquí:
// `kling version` tiene que funcionar sin daemon.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	c := api.NewClient(hostOf(*host))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	info, err := c.Info(ctx)
	cancel()
	return writeVersion(os.Stdout, Version, c.Endpoint(), info, err, *asJSON)
}

func writeVersion(w io.Writer, cli, endpoint string, info *api.Info, infoErr error, asJSON bool) error {
	if asJSON {
		out := struct {
			CLI         string `json:"cli"`
			OS          string `json:"os"`
			Arch        string `json:"arch"`
			Endpoint    string `json:"endpoint"`
			Daemon      string `json:"daemon,omitempty"`
			DaemonError string `json:"daemon_error,omitempty"`
		}{CLI: cli, OS: runtime.GOOS, Arch: runtime.GOARCH, Endpoint: endpoint}
		if infoErr != nil {
			out.DaemonError = infoErr.Error()
		} else if info != nil {
			out.Daemon = info.Version
		}
		return json.NewEncoder(w).Encode(out)
	}
	// La primera línea no cambia: hay scripts que hacen `kling version | head -1`.
	fmt.Fprintf(w, "kling %s\n", cli)
	if infoErr != nil || info == nil {
		fmt.Fprintf(w, "daemon: not reachable at %s\n", endpoint)
		return nil
	}
	fmt.Fprintf(w, "daemon: %s (%s)\n", info.Version, endpoint)
	if a, b := strings.TrimPrefix(cli, "v"), strings.TrimPrefix(info.Version, "v"); a != b && a != "dev" && b != "dev" {
		fmt.Fprintln(w, "note: CLI and daemon differ; upgrade the older one (kling doctor)")
	}
	return nil
}

// ── kling help <comando> ──────────────────────────────────────────────────────

// helpAliases lleva un alias al nombre con el que sale en la ayuda.
var helpAliases = map[string]string{"volumes": "volume", "sandboxes": "sandbox"}

// flagHelp son los comandos del núcleo cuyo `-h` solo imprime sus flags. Es
// una lista y no "todos" porque `kling help` los ejecuta: los que tienen
// subcomandos tratarían -h como uno desconocido, `status` lo ignora y
// consulta al daemon, y dial-stdio se quedaría sirviendo.
var flagHelp = map[string]bool{
	"run": true, "ps": true, "logs": true, "freeze": true, "thaw": true, "pause": true,
	"stop": true, "rm": true, "squeeze": true, "mmds": true, "commit": true, "rmi": true,
	"topo": true, "top": true, "events": true, "info": true, "inspect": true, "exec": true,
	"shell": true, "cp": true, "resize": true, "up": true, "daemon": true, "doctor": true,
	"try": true, "version": true,
}

// cmdHelp es `kling help <comando>`: solo el bloque de ese comando en la ayuda
// y, si tiene flags, la lista de `kling <comando> -h`. Una extensión responde
// ella misma, como siempre.
func cmdHelp(cmd string) error {
	name := cmd
	if a, ok := helpAliases[cmd]; ok {
		name = a
	}
	if block := usageBlock(name, usageHead+usageTail); block != "" {
		fmt.Print(block)
		if flagHelp[name] {
			if out := selfHelp(name); out != "" {
				fmt.Printf("\nFLAGS\n%s", out)
			}
		}
		return nil
	}
	if p := extensions().Lookup(cmd); p != nil {
		return plugin.Exec(p, cmd, []string{"-h"}, config.Path())
	}
	if ext, ok := movedToExtension[cmd]; ok {
		return movedError(cmd, ext)
	}
	return &errConCodigo{code: 2, err: &errWithHint{err: fmt.Errorf("unknown command %q", cmd), hint: "kling help"}}
}

// usageBlock saca de la ayuda las líneas de un comando: las que empiezan por
// "  <cmd> " (o son "  <cmd>") y sus continuaciones, más sangradas. Un comando
// puede salir en varias secciones (run también está en GOLDEN SNAPSHOTS) y se
// devuelven todas.
func usageBlock(cmd, text string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(text, "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 2:
			f := strings.Fields(line)
			in = len(f) > 0 && f[0] == cmd
		case indent >= 4 && strings.TrimSpace(line) != "":
			// continuación: sigue el estado del último comando
		default:
			in = false
		}
		if in {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// selfHelp ejecuta `kling <cmd> -h` en otro proceso y devuelve su lista de
// flags. En otro proceso porque los flagsets del núcleo salen con os.Exit al
// ver -h. Se acota el tiempo y el tamaño: es ayuda, no puede colgar.
func selfHelp(cmd string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out limitedBuffer
	out.max = 64 << 10
	c := exec.CommandContext(ctx, exe, cmd, "-h")
	c.Stdout, c.Stderr = &out, &out
	if c.Run() != nil && !strings.Contains(out.String(), "  -") {
		return ""
	}
	var keep []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "Usage of ") {
			continue
		}
		keep = append(keep, l)
	}
	s := strings.TrimRight(strings.Join(keep, "\n"), "\n")
	if !strings.Contains(s, "  -") {
		return ""
	}
	return s + "\n"
}

// limitedBuffer guarda hasta max bytes y descarta el resto sin fallar: un
// escritor que falla haría que el hijo recibiera SIGPIPE.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.Len(); room > 0 {
		if len(p) > room {
			l.Buffer.Write(p[:room])
		} else {
			l.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// errWithHint es un error que ya sabe su pista.
type errWithHint struct {
	err  error
	hint string
}

func (e *errWithHint) Error() string { return e.err.Error() }
func (e *errWithHint) Unwrap() error { return e.err }

// movedError es el error de un comando que vive en una extensión no instalada.
func movedError(cmd, ext string) error {
	return &errConCodigo{code: 2, err: &errWithHint{
		err:  fmt.Errorf("kling %s is provided by the kling-%s extension, which is not installed", cmd, ext),
		hint: "kling plugins install " + ext,
	}}
}
