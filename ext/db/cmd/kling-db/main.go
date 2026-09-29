// kling-db es la extensión de kling para bases de datos Postgres desechables:
// `kling db up`, `kling db fork`, `kling db connect`...
//
// Modelo: la base y el agente viven en la MISMA microVM. Una copia es una
// máquina instanciada (`run -from`) o ramificada (`sandbox fork`) de una
// plantilla con Postgres 16 ya caliente (scripts/db-golden.sh). Dentro, el
// agente usa el socket unix; desde el host, `kling db connect`.
//
// Lo que esta extensión añade a `kling run -from` es la higiene de credenciales:
// cada copia estrena contraseña antes de darse por lista, la clave solo existe
// en el host y connect solo la entrega para una copia lista y propia. Ver
// docs/db.md.
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
		Name:            "db",
		Version:         strings.TrimPrefix(Version, "v"),
		// sandbox fork con el guardián de credenciales y kling.ports.
		MinKling:  "0.16.0",
		Summary:   "disposable Postgres databases, one per microVM",
		HelpGroup: "SERVE",
		Commands: []plugin.Command{
			{Name: "up", Group: "COPIES", Summary: "a ready copy of a Postgres template",
				Usage: usage("up <template> [-name N] [-ttl D] [-owner T]", "a new copy with its own password")},
			{Name: "fork", Group: "COPIES", Summary: "branches a live copy into N copies",
				Usage:       usage("fork <copy> [-n N]", "N copies of a copy, all or none"),
				MachineArgs: []string{""}},
			{Name: "connect", Group: "COPIES", Summary: "how to reach a copy from the host",
				Usage:       usage("connect <copy> [-role R] [-dsn | -psql]", "address, or DSN, or a psql session"),
				MachineArgs: []string{""}},
			{Name: "attach", Group: "COPIES", Summary: "shares a copy with an agent in another machine",
				Usage:       usage("attach <agent> <copy> [-role R] [-env PGPASSWORD]", "through the proxy; the agent never sees the password"),
				MachineArgs: []string{""}},
			{Name: "detach", Group: "COPIES", Summary: "withdraws an attach and cuts its sessions",
				Usage:       usage("detach <agent> <copy> [-env PGPASSWORD]", "the agent no longer reaches the copy"),
				MachineArgs: []string{""}},
			{Name: "role", Group: "COPIES", Summary: "a read-only role inside a copy",
				Usage:       usage("role <copy> -ro [-name agent] [-schemas a,b] [-rm]", "a login role that can only read"),
				MachineArgs: []string{""}},
			{Name: "branch", Group: "COPIES", Summary: "one database per git branch",
				Usage: usage("branch [<branch>] [-from P] [-golden G]", "the copy of a git branch, forked from its parent") +
					usage("branch -switch | -ls | -rm B | -prune", "activate this branch's copy (git hook), list, clean up") +
					usage("branch hook install|uninstall", "post-checkout hook that switches the copy")},
			{Name: "reset", Group: "COPIES", Summary: "replaces a copy with a fresh one from its template",
				Usage:       usage("reset <copy>", "same name, same template, new data"),
				MachineArgs: []string{""}},
			{Name: "rm", Group: "COPIES", Summary: "removes a copy and its password",
				Usage:       usage("rm <copy>", "the machine and its password file"),
				MachineArgs: []string{""}},
			{Name: "rehearse", Group: "OPERATE", Summary: "rehearses SQL migrations on a throwaway copy",
				Usage:       usage("rehearse <copy|template> -migrations DIR", "timings, lock waits and size per file"),
				MachineArgs: []string{""}},
			{Name: "rotate", Group: "OPERATE", Summary: "gives a copy a new password",
				Usage:       usage("rotate <copy>", "new password; the old one stays if it fails"),
				MachineArgs: []string{""}},
			{Name: "snapshot", Group: "OPERATE", Summary: "saves a copy as a restore point",
				Usage:       usage("snapshot [-rm] <copy> <name>", "a restore point of a live copy"),
				MachineArgs: []string{""}},
			{Name: "snapshots", Group: "OPERATE", Summary: "lists the restore points of a copy",
				Usage:       usage("snapshots <copy> [-json]", "the restore points of a copy"),
				MachineArgs: []string{""}},
			{Name: "undo", Group: "OPERATE", Summary: "returns a copy to a restore point",
				Usage:       usage("undo <copy> [<name>]", "same name and owner, new password"),
				MachineArgs: []string{""}},
			{Name: "ask", Group: "COPIES", Summary: "a question in plain words, answered read-only",
				Usage:       usage(`ask <copy> "question" [-yes] [-role R]`, "answered by a model, run read-only"),
				MachineArgs: []string{""}},
			{Name: "doctor", Group: "DIAGNOSE", Summary: "checks a copy or a Postgres URL",
				Usage:       usage("doctor <copy> | -url postgres://...", "finds what is wrong with a database"),
				MachineArgs: []string{""}},
			{Name: "audit", Group: "DIAGNOSE", Summary: "timeline of a copy: events and connections",
				Usage:       usage("audit <copy> [-since 24h] [-json]", "who connected, and when"),
				MachineArgs: []string{""}},
			{Name: "golden", Group: "TEMPLATES", Summary: "builds the pg16 image and Postgres templates",
				Usage: usage("golden image", "builds the pg16 image") +
					usage("golden build [opts] <name>", "a warm Postgres frozen as a template") +
					usage("golden build -template T <name>", "the same, from a bundled template"),
				Subcommands: []string{"image", "build"}},
			{Name: "clone", Group: "TEMPLATES", Summary: "a golden from a production database, personal data masked",
				Usage: usage("clone <postgres-url> -mask RULES [-golden G]", "read-only, dumped and masked inside a microVM")},
			{Name: "templates", Group: "TEMPLATES", Summary: "lists the bundled database templates",
				Usage: usage("templates", "empty, crm-demo...")},
		},
	}
}

// usage es una línea del bloque de ayuda con las columnas del núcleo.
func usage(cmd, desc string) string { return fmt.Sprintf("  db %-45s %s\n", cmd, desc) }

func main() {
	plugin.Main(manifest(), map[string]func([]string) error{
		"up":        cmdUp,
		"fork":      cmdFork,
		"branch":    cmdBranch,
		"connect":   cmdConnect,
		"attach":    cmdAttach,
		"detach":    cmdDetach,
		"reset":     cmdReset,
		"rm":        cmdRm,
		"rehearse":  cmdRehearse,
		"rotate":    cmdRotate,
		"snapshot":  cmdSnapshot,
		"snapshots": cmdSnapshots,
		"undo":      cmdUndo,
		"doctor":    cmdDoctor,
		"audit":     cmdAudit,
		"ask":       cmdAsk,
		"golden":    cmdGolden,
		"clone":     cmdClone,
		"role":      cmdRole,
		"templates": cmdTemplates,
	}, map[string]func([]string, io.Writer) error{})
}
