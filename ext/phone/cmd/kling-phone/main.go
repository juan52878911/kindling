// kling-phone es la extensión de kling para teléfonos Android (Redroid) en
// microVMs: `kling phone up`, `kling phone view`, `kling phone mcp`...
//
// Todo va por la API del daemon (pkg/api), nunca por dentro de la máquina: sin
// allow_exec. Cada teléfono es `run -from` de un dorado (Android arrancado y
// congelado, `kling phone golden build`); lo que añade la extensión es:
//
//   - la identidad de cada clon (android_id, serie, SSAID, claves de adb) y su
//     token de la API del teléfono, por MMDS y los ganchos de la imagen
//     (`machine secret -hooks`), como phone.sh;
//   - la API de kling-phoned (8091 del invitado) por el proxy del daemon,
//     con el token del clon (docs/phoned.md, #110);
//   - un muro web con las pantallas (`view`) y un servidor MCP (`mcp`) que da
//     un teléfono por sesión, para que `kling mcp link` lo importe.
//
// El estado de la extensión vive en el daemon: etiquetas en las máquinas, el
// token de cada clon en el store (ns "phone") y el resultado de construir y
// verificar el dorado en una anotación de su snapshot.
package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func manifest() plugin.Manifest {
	return plugin.Manifest{
		ManifestVersion: plugin.ManifestVersion,
		Name:            "phone",
		Version:         strings.TrimPrefix(Version, "v"),
		// run -wait-ready, POST /hooks y la marca de secretos que se levanta.
		MinKling:  "0.16.0",
		Summary:   "Android phones in microVMs: identity per clone, a screen wall and an MCP server",
		HelpGroup: "SERVE",
		Commands: []plugin.Command{
			{Name: "up", Group: "PHONES", Summary: "N new phones from the golden",
				Usage: usage("up [-n N] [-golden G]", "N phones, each with its own identity (builds the golden if missing)")},
			{Name: "ls", Group: "PHONES", Summary: "phones, state, adb address and API health",
				Usage: usage("ls [-json]", "name, state, adb address and whether its API answers")},
			{Name: "view", Group: "PHONES", Summary: "a web wall with every screen, touch and keys",
				Usage: usage("view [-listen 127.0.0.1:8765] [-open]", "every screen; tap, swipe, keys and health")},
			{Name: "adb", Group: "PHONES", Summary: "adb against a phone (resolves the forward)",
				Usage:       usage("adb <phone> [adb args...]", "without args prints the adb address"),
				MachineArgs: []string{""}},
			{Name: "api", Group: "PHONES", Summary: "one call to the phone API (kling-phoned)",
				Usage:       usage("api <phone> <METHOD> <path> [body-file|-]", "e.g. api 1 GET /v1/health; binary with ?encoding=base64"),
				MachineArgs: []string{""}},
			{Name: "pause", Group: "PHONES", Summary: "pauses phones in RAM (no CPU; resume in ms)",
				Usage:       usage("pause <phone>...", "paused in RAM"),
				MachineArgs: []string{""}},
			{Name: "resume", Group: "PHONES", Summary: "resumes paused or frozen phones",
				Usage:       usage("resume <phone>...", "thaw and wait for its API"),
				MachineArgs: []string{""}},
			{Name: "rm", Group: "PHONES", Summary: "removes phones and their API tokens",
				Usage:       usage("rm <phone>... | -a", "the machines and their tokens"),
				MachineArgs: []string{""}},
			{Name: "pool", Group: "PHONES", Summary: "N spare phones, paused and ready",
				Usage: usage("pool <N> [-watch] [-freeze-after 30m]", "prewarmed spares; -watch refills and freezes idle ones")},
			{Name: "adopt", Group: "PHONES", Summary: "gives identity and API token to a phone kling phone did not make",
				Usage:       usage("adopt <machine>", "e.g. a graph node from the golden: its API stays locked until then"),
				MachineArgs: []string{""}},
			{Name: "token", Group: "PHONES", Summary: "the phone API token, to hand to a graph node",
				Usage:       usage("token <phone> [-read] [-rotate]", "prints a token for the 8091 (docs/phoned.md)"),
				MachineArgs: []string{""}},
			{Name: "golden", Group: "GOLDEN", Summary: "builds and verifies the golden phone",
				Usage: usage("golden build [-image I] [-name G]", "cold boot, ready probe, page-cache check, save") +
					usage("golden verify [-name G]", "a throwaway clone: health and page cache again") +
					usage("golden inspect [-name G] [-json]", "what was recorded when it was built and verified"),
				Subcommands: []string{"build", "verify", "inspect"}},
			{Name: "mcp", Group: "MCP", Summary: "the phones as an MCP server, one phone per session",
				Usage: usage("mcp [-listen 127.0.0.1:8095] [-phone P]", "phone.screen, phone.tree, phone.tap... for kling mcp link")},
		},
		Config: []plugin.ConfigKey{
			{Key: "image", Type: "string", Help: "Android image for the golden (default android13)"},
			{Key: "golden", Type: "string", Help: "golden template name (default phone-golden)"},
			{Key: "prefix", Type: "string", Help: "phone names: <prefix>-N (default phone)"},
			{Key: "cpus", Type: "int", Help: "vCPUs of the golden (default 2)"},
			{Key: "mem", Type: "int", Help: "MiB of the golden (default 1536)"},
			{Key: "egress", Type: "string", Help: "egress of the golden: none | internet (default none)"},
			{Key: "adb_pubkey", Type: "string", Help: "adb public key every phone accepts (default ~/.android/adbkey.pub)"},
			{Key: "adb_keys_dir", Type: "string", Help: "directory with <phone>.pub: that phone's own adb key"},
			{Key: "min_memlevel", Type: "int", Help: "macOS: do not start a phone below this kern.memorystatus_level (default 35)"},
		},
	}
}

// usage es una línea del bloque de ayuda con las columnas del núcleo.
func usage(cmd, desc string) string { return fmt.Sprintf("  phone %-44s %s\n", cmd, desc) }

func main() {
	plugin.Main(manifest(), map[string]func([]string) error{
		"up":     cmdUp,
		"ls":     cmdLs,
		"view":   cmdView,
		"adb":    cmdAdb,
		"api":    cmdAPI,
		"pause":  cmdPause,
		"resume": cmdResume,
		"rm":     cmdRm,
		"pool":   cmdPool,
		"token":  cmdToken,
		"adopt":  cmdAdopt,
		"golden": cmdGolden,
		"mcp":    cmdMCP,
	}, map[string]func([]string, io.Writer) error{})
}
