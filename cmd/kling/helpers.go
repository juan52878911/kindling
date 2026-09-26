package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
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
		hint: "kling plugin install " + ext,
	}}
}
