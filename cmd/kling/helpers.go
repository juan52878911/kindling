package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
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
// daemon, su API, su VMM y las versiones del agente de invitado de las
// máquinas que lo dijeron. El desfase entre CLI y daemon se descubría por un
// 404 en un comando nuevo; así se ve antes. Un daemon que no contesta no es
// un error aquí: `kling version` tiene que funcionar sin daemon.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	c := api.NewClient(hostOf(*host))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := c.Info(ctx)
	var agentes []string
	if err == nil {
		agentes = versionesAgente(ctx, c)
	}
	return writeVersion(os.Stdout, Version, c.Endpoint(), info, agentes, err, *asJSON)
}

// versionesAgente son las versiones distintas del agente de invitado que
// anunciaron las máquinas del daemon (Machine.Agent), ordenadas. Vacío si
// ninguna lo dijo o el daemon no contesta a tiempo: es "si se conoce".
func versionesAgente(ctx context.Context, c *api.Client) []string {
	ms, err := c.List(ctx)
	if err != nil {
		return nil
	}
	vistas := map[string]bool{}
	var out []string
	for _, m := range ms {
		if m.Agent != nil && m.Agent.Version != "" && !vistas[m.Agent.Version] {
			vistas[m.Agent.Version] = true
			out = append(out, m.Agent.Version)
		}
	}
	sort.Strings(out)
	return out
}

// vmmDe es el VMM del daemon y su versión ("firecracker", "Firecracker v1.17").
func vmmDe(info *api.Info) (string, string) {
	vmm := info.Backend
	if vmm == "" {
		vmm = "firecracker" // daemon anterior a v0.9: siempre lo era
	}
	return vmm, strings.TrimSpace(info.Firecrack)
}

// versionJSON es la salida de `kling version -json` (testdata/contrato/json.txt).
type versionJSON struct {
	CLI         string   `json:"cli"`
	API         int      `json:"api"` // el APIVersion que habla este CLI
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Endpoint    string   `json:"endpoint"`
	Daemon      string   `json:"daemon,omitempty"`
	DaemonAPI   int      `json:"daemon_api,omitempty"`
	VMM         string   `json:"vmm,omitempty"`
	VMMVersion  string   `json:"vmm_version,omitempty"`
	Agents      []string `json:"agents,omitempty"`
	DaemonError string   `json:"daemon_error,omitempty"`
}

func writeVersion(w io.Writer, cli, endpoint string, info *api.Info, agentes []string, infoErr error, asJSON bool) error {
	if asJSON {
		out := versionJSON{CLI: cli, API: api.APIVersion, OS: runtime.GOOS, Arch: runtime.GOARCH, Endpoint: endpoint}
		if infoErr != nil {
			out.DaemonError = infoErr.Error()
		} else if info != nil {
			out.Daemon = info.Version
			out.DaemonAPI = info.DaemonAPI()
			out.VMM, out.VMMVersion = vmmDe(info)
			out.Agents = agentes
		}
		return json.NewEncoder(w).Encode(out)
	}
	// La primera línea no cambia: hay scripts que hacen `kling version | head -1`.
	fmt.Fprintf(w, "kling %s\n", cli)
	if infoErr != nil || info == nil {
		fmt.Fprintf(w, "daemon: not reachable at %s\n", endpoint)
		fmt.Fprintf(w, "api:    %d\n", api.APIVersion)
		return nil
	}
	fmt.Fprintf(w, "daemon: %s (%s)\n", info.Version, endpoint)
	fmt.Fprintf(w, "api:    %d (daemon %d)\n", api.APIVersion, info.DaemonAPI())
	vmm, vmmVer := vmmDe(info)
	fmt.Fprintf(w, "vmm:    %s\n", strings.TrimSpace(vmm+" "+vmmVer))
	if len(agentes) > 0 {
		fmt.Fprintf(w, "agent:  %s (as announced by its machines)\n", strings.Join(agentes, ", "))
	}
	if a, b := strings.TrimPrefix(cli, "v"), strings.TrimPrefix(info.Version, "v"); a != b && a != "dev" && b != "dev" {
		fmt.Fprintln(w, "note: CLI and daemon differ; upgrade the older one (kling doctor)")
	}
	return nil
}

// flagRetirado registra un flag retirado en 1.0 para que quien lo use lea el
// nombre nuevo: el flag sale con el código de uso (2) y "-viejo was removed
// in 1.0: use -nuevo", en vez de un "flag provided but not defined".
func flagRetirado(fs *flag.FlagSet, viejo, nuevo string) {
	fs.Func(viejo, "removed in 1.0: use -"+nuevo, func(string) error {
		return fmt.Errorf("-%s was removed in 1.0: use -%s", viejo, nuevo)
	})
}

// errAgenteSinCapacidad es que la máquina pedía algo que el agente de su
// imagen no sabe hacer (una imagen anterior a esa capacidad): el daemon la da
// por fallida. Sale con el código 5 (codigoDeSalida).
type errAgenteSinCapacidad struct {
	maquina string
	faltan  []string
}

func (e *errAgenteSinCapacidad) Error() string {
	return fmt.Sprintf("%s: its guest agent lacks %s (an image built before that capability): rebuild or reimport the image",
		e.maquina, strings.Join(e.faltan, ", "))
}

// agenteSinCapacidad mira, con lo que el agente anunció (mc.Agent), si le
// falta algo que req necesita: las mismas reglas con las que el daemon falla
// la máquina (internal/machine/entorno.go, volumen_espera.go). nil si no le
// falta nada o si todavía no se sabe (agente sin contestar).
func agenteSinCapacidad(req api.RunRequest, mc *api.Machine) error {
	if mc == nil || mc.Agent == nil {
		return nil
	}
	var faltan []string
	for _, c := range []struct {
		pide bool
		cap  string
	}{
		{len(req.Env) > 0, api.GuestCapEnv},
		{len(req.Cmd) > 0 || len(req.Entrypoint) > 0, api.GuestCapCmd},
		{req.VolumeWait, api.GuestCapVolumeWait},
	} {
		if c.pide && !mc.Agent.Has(c.cap) {
			faltan = append(faltan, c.cap)
		}
	}
	if len(faltan) == 0 {
		return nil
	}
	return &errAgenteSinCapacidad{maquina: config.Or(mc.Name, mc.ID), faltan: faltan}
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
