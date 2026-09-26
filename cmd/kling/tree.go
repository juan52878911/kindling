package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// El árbol de comandos de kling: una sola fuente para la ayuda, el completado
// y el enrutado.
//
// Tres sustantivos —imagen (rootfs, arranca en frío) → plantilla (snapshot
// dorado, arranca en ms) → máquina— y doce verbos de uso diario en primer
// nivel; lo demás vive bajo su sustantivo (template, image, volume, machine,
// context, config, plugin) o bajo la extensión que lo aporta (ai, mcp). Los
// nombres de antes siguen funcionando como alias silenciosos (ver aliases).

// section es un grupo de comandos en `kling help all`.
type section struct {
	title string
	cmds  []plugin.Command
	// advanced es que no sale en `kling help` ni en el completado de primer
	// nivel: internos y lo que solo teclea quien ya sabe (machine, daemon…).
	advanced bool
}

// coreTree son los comandos del núcleo, en el orden en que salen.
var coreTree = []section{
	{title: "START HERE", cmds: []plugin.Command{
		{Name: "up", Summary: "gets the runtime ready and starts the daemon", Usage: `  up [-check]                                      gets the runtime ready: KVM,
                                                   nftables, user, images, daemon
                                                   and extension units
`},
		{Name: "status", Summary: "which piece is up and which is missing", Usage: `  status [-v] [-json]                              which piece is up and which is
                                                   missing (-v: daemon details)
`},
		{Name: "doctor", Summary: "checks runtime, daemon, versions, extensions and completion", Usage: `  doctor [-json]                                   checks runtime, daemon, versions,
                                                   extensions and completion; prints
                                                   the fix for each ✗
`},
		{Name: "try", Summary: "runs a command in a throwaway microVM and removes it", MachineArgs: nil, Usage: `  try [-image I | -from T] [-mem 256M]             runs a command in a throwaway
      [-egress none|internet|allowlist] [-keep]    microVM and removes it (no
      [--] [cmd [args...]]                         command: a shell); exits with
                                                   the command's exit code
`},
		{Name: "help", Summary: "help for one command, with its flags", Subcommands: []string{"all"}, Usage: `  help [<command> [<subcommand>]] | all            help for one command, with its
                                                   flags; all: every command
`},
	}},
	{title: "MACHINES", cmds: []plugin.Command{
		{Name: "run", Summary: "creates and starts a microVM", Usage: `  run [-name N] [-image I | -from T]               creates and starts a microVM from
      [-cpus N] [-mem 256M]                        an image (cold boot) or a template
      [-egress none|internet|allowlist]            (~ms); network egress (default:
      [-allow dom1,dom2]                           none) and domains allowed
      [-ttl 10m] [-cpu-pct PCT]                    auto-freeze and CPU ceiling
      [-service NAME] [-label k=v]                 grouping by service
      [-volume NAME[:/mount][:ro]] (repeatable)    storage that survives the machine
      [-allow-exec] [-on-ttl freeze|remove]        accept exec/cp; remove instead of
                                                   freezing
      [-mem-max 2G]                                ceiling for machine resize
      [-share SRC:DST[:copy|ro|rw]] (repeatable)   host folder inside: a read-only
                                                   copy (default), or live
`},
		{Name: "ps", Summary: "lists the machines", Usage: `  ps [-a] [-q] [-json]                             lists the machines (-a: stopped
                                                   too; -q: only IDs)
`},
		{Name: "inspect", Summary: "a machine in JSON", MachineArgs: []string{""}, Usage: `  inspect <ref>                                    a machine in JSON, shares included
`},
		{Name: "logs", Summary: "microVM serial console", MachineArgs: []string{""}, Usage: `  logs [-f] [-tail N] <ref>                        microVM serial console (-f: follow)
`},
		{Name: "freeze", Summary: "freezes into a snapshot: 0 RAM, thaws in ms", MachineArgs: []string{""}, Usage: `  freeze <ref>...                                  dumps it to a snapshot -> frozen
                                                   (no CPU, no RAM; thaw ~30 ms)
`},
		{Name: "thaw", Summary: "restores a frozen or paused machine", MachineArgs: []string{""}, Usage: `  thaw <ref>...                                    restores from its snapshot (~ms),
                                                   or resumes a paused one
`},
		{Name: "pause", Summary: "pauses it without dumping memory", MachineArgs: []string{""}, Usage: `  pause <ref>...                                   pauses it keeping its RAM (thaw
                                                   resumes it in ~1 ms)
`},
		{Name: "stop", Summary: "terminates the machine", MachineArgs: []string{""}, Usage: `  stop <ref>...                                    terminates the machine
`},
		{Name: "rm", Summary: "removes machine and snapshot", MachineArgs: []string{""}, Usage: `  rm [-f] <ref>...                                 removes machine and snapshot
                                                   (asks first for several; -f: no)
`},
		{Name: "save", Summary: "turns a machine into a template", MachineArgs: []string{""}, Usage: `  save [-replace] [-force] <ref> <name>            freezes a machine as a reusable
                                                   template: run -from <name>
`},
	}},
	{title: "INSIDE A MACHINE", cmds: []plugin.Command{
		{Name: "exec", Summary: "runs a command inside, streaming its output", MachineArgs: []string{""}, Usage: `  exec [-i] [-e K=V] [-w DIR] [-timeout D]         runs a command inside, streaming
      <ref> [--] <cmd> [args...]                   its output; exits with its code
`},
		{Name: "shell", Summary: "interactive terminal inside", MachineArgs: []string{""}, Usage: `  shell [-e K=V] [-w DIR] [-t TERM] <ref>          interactive terminal inside
      [--] [cmd [args...]]                         (Ctrl-C reaches the program)
`},
		{Name: "cp", Summary: "copies files in and out of a machine", Usage: `  cp <local|-> <ref>:<path>                        copies a file into a machine
  cp <ref>:<path> <local|->                        ... or out of it
`},
		{Name: "sandbox", Summary: "throwaway microVMs that run code", Subcommands: []string{"create", "ls", "renew", "rm"}, MachineArgs: []string{"renew", "rm"}, Usage: `  sandbox create [-image I | -from T] [-ttl 10m]   a throwaway microVM that runs code:
      [-egress none|internet|allowlist]            no network by default; when idle it
      [-on-ttl remove|freeze] [-q]                 is destroyed, or frozen at zero cost
      [-mem 256M] [-cpus N] [-volume ...]          and woken by the next exec
      [-share SRC:DST[:copy|ro|rw]]                host folder inside, as in run
  sandbox ls [-q] [-json]                          lists them
  sandbox renew <sb> [-ttl 10m]                    extends its lifetime
  sandbox rm [-f] <sb>...                          destroys them
`},
	}},
	{title: "TEMPLATES (golden snapshots)", cmds: []plugin.Command{
		{Name: "template", Summary: "reusable snapshots: run -from <name> starts in ms", Subcommands: []string{"ls", "inspect", "rm"}, Usage: `  template ls [-q] [-json]                         lists the templates
  template inspect <name> [-json]                  one template, with its annotations
  template rm [-f] <name>...                       removes templates
  save <ref> <name>                                makes one from a running machine
`},
	}},
	{title: "IMAGES (rootfs)", cmds: []plugin.Command{
		{Name: "image", Summary: "rootfs images: build, inspect, copy between daemons", Subcommands: []string{"ls", "build", "recipe", "cat", "put", "rm", "copy", "toolchain"}, Usage: `  image ls [-q] [-json]                            lists built rootfs images
  image build <name> -builder B [-spec f.json]     builds it with a builder installed
                                                   on the daemon (extensions ship them)
  image recipe <image>                             how it was built
  image cat <image> <path> [-stat]                 prints a file inside an image
  image put <image> <path> (-file F|-from-host N)  puts a file inside a built image
      [-mode 0755] [-create]
  image rm [-f] <image>...                         removes it (refuses if a layer, a
                                                   template or a machine uses it)
  image copy <image> -from H [-to H]               streams an image (and its kernel
                                                   and base) between daemons
  image toolchain                                  builds the image with npm and pip
                                                   (try and volume populate use it)
`},
	}},
	{title: "VOLUMES", cmds: []plugin.Command{
		{Name: "volume", Summary: "storage that survives the microVM", Subcommands: []string{"create", "ls", "rm", "populate"}, Usage: `  volume create <name> [-size 2G]                  storage that survives the microVM
  volume ls [-q] [-json]                           lists them
  volume rm [-f] <name>...                         removes them
  volume populate <name> [-image I] -- <cmd>       installs packages inside a microVM
`},
	}},
	{title: "OBSERVATION", cmds: []plugin.Command{
		{Name: "top", Summary: "memory per microVM (PSS) and the host", MachineArgs: []string{""}, Usage: `  top [-watch DUR] [-json]                         memory per microVM (PSS) and the
                                                   host; snapshot or refresh
`},
	}},
	{title: "CONFIGURATION", cmds: []plugin.Command{
		{Name: "context", Summary: "known daemons", Subcommands: []string{"ls", "add", "use", "rm"}, Usage: `  context [ls] [-json]                             lists known daemons
  context add <name> <host>                        adds one and activates it
  context use <name>                               switches daemon
  context rm <name>                                removes it
`},
		{Name: "config", Summary: "current configuration", Subcommands: []string{"show", "set", "path"}, Usage: `  config [show [-json]|path]                       current configuration
  config set <key> <value>                         e.g. defaults.image min
`},
		{Name: "plugin", Summary: "installed extensions", Subcommands: []string{"ls", "install", "rm", "enable", "disable"}, Usage: `  plugin [ls] [-json]                              installed extensions, their status
                                                   and source
  plugin install <name>[@vX.Y.Z]                   download from the kindling release
          [-from URL] [-file PATH] [-sha256 H]     (sha256-checked)
  plugin rm <name>                                 remove one installed by it
  plugin enable|disable <name>                     turn an extension (also a built-in
                                                   one) on or off
`},
		{Name: "completion", Summary: "shell completion script", Subcommands: []string{"bash", "zsh", "fish", "install"}, Usage: `  completion bash|zsh|fish                         shell completion script
  completion install [shell]                       writes it to ~/.config/kling and
                                                   prints the line for your shell's rc
`},
		{Name: "version", Summary: "CLI version, and the daemon's if it answers", Usage: `  version [-json]                                  CLI version, and the daemon's if
                                                   it answers
`},
	}},
	{title: "ADVANCED", advanced: true, cmds: []plugin.Command{
		{Name: "machine", Summary: "resize, squeeze, secrets", Subcommands: []string{"resize", "squeeze", "secret"}, MachineArgs: []string{"resize", "squeeze", "secret"}, Usage: `  machine resize <ref> -mem 512M                   changes its memory without
                                                   restarting, up to its -mem-max
  machine squeeze <ref>...                         balloon: returns the guest's free
                                                   memory to the host
  machine secret <ref> [-f store.json]             injects a session secret via MMDS
                                                   (stdin if no -f); it can no longer
                                                   be frozen
`},
		{Name: "topo", Summary: "ASCII diagram of everything", Usage: `  topo                                             ASCII diagram of everything
`},
		{Name: "events", Summary: "stream of daemon events", Usage: `  events [-json]                                   stream of daemon events
`},
		{Name: "daemon", Summary: "starts the core", Usage: `  daemon [-socket S] [-root R] [-firecracker BIN]  starts the core (VMM: config
                                                   daemon.vmm, or $KLING_VMM)
`},
		{Name: "dial-stdio", Summary: "remote end of the SSH transport", Hidden: true},
		{Name: "builder", Summary: "runs an image builder (the daemon calls it)", Hidden: true},
	}},
}

const usageTail = `CONNECTION
  Precedence:  -H  >  $KLING_HOST  >  active context  >  local socket

    kling context add lab ssh://juan@192.168.2.60
    kling context use lab

  The daemon never listens on a network port: controlling microVMs is
  equivalent to root on its host, so the only remote access is SSH.
`

// aliases son los nombres de antes: se traducen en silencio a los de ahora
// antes de enrutar. No salen en la ayuda ni en el completado.
//
// Plan: en 0.14 son silenciosos; en 0.15 avisan una vez por proceso en stderr
// ("warning: kling add is now kling mcp add"), como hizo -cpu; en 0.16 se
// retiran los de extensiones (add, search, gateway, export, memory, migrate,
// models, chispa, info). commit, snapshots y plugins se quedan para siempre:
// cuestan cero y hay scripts ajenos que los usan.
var aliases = map[string]string{
	"commit":    "save",
	"snapshots": "template",
	"rmi":       "template rm",
	"images":    "image",
	"plugins":   "plugin",
	"volumes":   "volume",
	"sandboxes": "sandbox",
	"info":      "status -v",
	"resize":    "machine resize",
	"squeeze":   "machine squeeze",
	"mmds":      "machine secret",
	"models":    "ai model",
	"chispa":    "ai chispa",
}

// extAliases son los verbos que kling-mcp tenía sueltos hasta 0.13. No se
// reservan al núcleo: una kling-mcp de 0.13 (manifiesto v1) sigue siendo su
// dueña y los recibe tal cual; solo cuando nadie los sirve se traducen.
var extAliases = map[string]string{
	"add":     "mcp add",
	"search":  "mcp search",
	"gateway": "mcp serve",
	"export":  "mcp export",
	"memory":  "mcp memory",
	"migrate": "mcp migrate",
}

// resolveAlias traduce un comando viejo a (comando, args) de ahora. Devuelve
// lo mismo que recibió si no es un alias, o si es un verbo de extensión que
// una extensión instalada todavía sirve.
func resolveAlias(cmd string, args []string) (string, []string) {
	to, ok := aliases[cmd]
	if !ok {
		if to, ok = extAliases[cmd]; !ok || extensions().Lookup(cmd) != nil {
			return cmd, args
		}
	}
	parts := strings.Fields(to)
	return parts[0], append(append([]string(nil), parts[1:]...), args...)
}

// coreCommands son las palabras de primer nivel del núcleo, alias incluidos.
// Ganan siempre: una extensión que declare una de estas no la recibe.
var coreCommands = func() []string {
	var out []string
	for _, s := range coreTree {
		for _, c := range s.cmds {
			out = append(out, c.Name)
		}
	}
	for a := range aliases {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}()

// coreCommand devuelve el comando del núcleo de ese nombre, o nil.
func coreCommand(name string) *plugin.Command {
	for si := range coreTree {
		for ci := range coreTree[si].cmds {
			if coreTree[si].cmds[ci].Name == name {
				return &coreTree[si].cmds[ci]
			}
		}
	}
	return nil
}

// subBlock saca del bloque de uso de un sustantivo las líneas de uno de sus
// subcomandos ("  template rm ..." y sus continuaciones). Si el bloque lleva
// delante el nombre de la extensión ("  ai model add"), ns es esa palabra.
func subBlock(c *plugin.Command, sub string, ns ...string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.SplitAfter(c.Usage, "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 2:
			f := strings.Fields(line)
			if len(ns) > 0 && len(f) > 0 && f[0] == ns[0] {
				f = f[1:]
			}
			in = len(f) > 1 && f[0] == c.Name && f[1] == sub
		case indent >= 4 && strings.TrimSpace(line) != "":
			// continuación: sigue el estado del último subcomando
		default:
			in = false
		}
		if in {
			b.WriteString(line)
		}
	}
	return b.String()
}

// hasSub dice si sub es un subcomando de c.
func hasSub(c *plugin.Command, sub string) bool {
	for _, s := range c.Subcommands {
		if s == sub {
			return true
		}
	}
	return false
}

// shortVersion es "0.14" para la cabecera: sin la v ni el parche.
func shortVersion() string {
	v := strings.TrimPrefix(Version, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if p := strings.Split(v, "."); len(p) >= 3 {
		v = p[0] + "." + p[1]
	}
	return v
}

// printUsage es la pantalla de `kling` a secas y de `kling help`: dónde
// empezar, lo de cada día y a dónde ir a buscar lo demás. El volcado completo
// es `kling help all`.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, "kling %s — Firecracker microVMs that wake in milliseconds\n\n", shortVersion())
	fmt.Fprint(w, `START HERE
  kling try -- uname -a        run a command in a throwaway microVM
  kling mcp add <server>       host an MCP server as a frozen service
  kling connect                plug your agent (Claude Code, opencode, Cursor…) into it

EVERY DAY
  run, ps, logs, exec, shell, cp        machines and what runs inside
  freeze, thaw, pause, stop, rm         lifecycle (frozen = 0 RAM, thaw ~30 ms)
  save <ref> <name>                     turn a machine into a template

MANAGE     template, image, volume, sandbox, context, config, plugin
`)
	// Las extensiones (incorporadas o instaladas), por la sección que declara
	// cada una: una línea por espacio de nombres y una por verbo promovido
	// (connect ya está arriba). SERVE va antes que EXTENSIONS: es lo de casa.
	groups := map[string][]string{}
	var order []string
	add := func(g, line string) {
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], line)
	}
	reg := extensions()
	for _, p := range reg.Plugins {
		if p.Err != nil {
			continue
		}
		if reg.IsNamespace(p.Name) {
			add(p.Manifest.Group(), fmt.Sprintf("  %-10s %s", p.Name, p.Manifest.Summary))
		}
		for _, c := range p.Manifest.Promoted() {
			if reg.Lookup(c.Name) == p && !c.Hidden && c.Name != "connect" {
				add(p.Manifest.Group(), fmt.Sprintf("  %-10s %s", c.Name, c.Summary))
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if (order[i] == "SERVE") != (order[j] == "SERVE") {
			return order[i] == "SERVE"
		}
		return order[i] < order[j]
	})
	for _, g := range order {
		fmt.Fprintln(w, g)
		for _, l := range groups[g] {
			fmt.Fprintln(w, l)
		}
	}
	fmt.Fprint(w, `CHECK      status, doctor, top, version

kling help <command>     kling help all     docs: docs/README.md
`)
}

// printUsageAll es `kling help all`: todo, como antes, sección a sección.
func printUsageAll(w io.Writer) {
	fmt.Fprintf(w, "kling %s — Firecracker microVMs with a docker-style interface\n\nUSAGE\n  kling <command> [options]\n\n", shortVersion())
	for _, s := range coreTree {
		fmt.Fprintln(w, s.title)
		plugin.WriteCommands(w, "", s.cmds)
		fmt.Fprintln(w)
	}
	for _, p := range extensions().Namespaces() {
		m := p.Manifest
		fmt.Fprintf(w, "%s — %s\n", strings.ToUpper(p.Name), m.Summary)
		plugin.WriteGrouped(w, p.Name+" ", m.Namespaced(), "")
		fmt.Fprintln(w)
	}
	var promoted []plugin.Command
	for _, c := range extensions().Commands() {
		if !extensions().IsNamespace(c.Name) {
			promoted = append(promoted, c)
		}
	}
	if len(promoted) > 0 {
		plugin.WriteHelp(w, promoted)
	}
	fmt.Fprint(w, usageTail)
}

// noFlagHelp son los comandos a los que no se les pide `-h` en otro proceso
// para sacar sus flags: no tienen, o hacer eso los ejecutaría.
var noFlagHelp = map[string]bool{"help": true, "completion": true, "dial-stdio": true, "builder": true, "cp": true}

// cmdHelp es `kling help [<command> [<subcommand>]]` y también a donde llega
// `kling <command> -h`. Un solo formato para el núcleo y las extensiones: la
// sinopsis y el bloque salen del árbol o del manifiesto, y los flags del
// propio comando en otro proceso (plugin.FlagHelp).
func cmdHelp(path []string) error {
	w := os.Stdout
	if len(path) == 0 {
		printUsage(w)
		return nil
	}
	if path[0] == "all" {
		printUsageAll(w)
		return nil
	}
	name, rest := resolveAlias(path[0], path[1:])
	self, _ := os.Executable()

	if c := coreCommand(name); c != nil {
		if len(rest) == 0 {
			flags := ""
			if len(c.Subcommands) == 0 && !noFlagHelp[name] {
				flags = plugin.FlagHelp(self, name)
			}
			plugin.WriteCommand(w, "kling "+name, "", *c, flags, "kling help all")
			return nil
		}
		if hasSub(c, rest[0]) {
			sub := plugin.Command{Name: name + " " + rest[0], Usage: subBlock(c, rest[0])}
			if sub.Usage == "" {
				sub.Usage = plugin.Line(sub.Name, "")
			}
			plugin.WriteCommand(w, "kling "+sub.Name, "", sub, plugin.FlagHelp(self, name, rest[0]), "kling help "+name)
			return nil
		}
		return unknownHelp(name + " " + rest[0])
	}

	if p := extensions().Lookup(name); p != nil {
		m := p.Manifest
		exe, base := p.Path, []string{}
		if p.Builtin != nil {
			exe = self
		}
		if extensions().IsNamespace(name) {
			if len(rest) == 0 {
				plugin.WriteNamespace(w, "kling "+name, *m)
				return nil
			}
			if p.Builtin != nil {
				base = []string{name}
			}
			return commandHelp(w, m, name+" ", "kling help "+name, exe, base, rest)
		}
		return commandHelp(w, m, "", "kling help all", exe, base, append([]string{name}, rest...))
	}
	if ext, ok := movedToExtension[name]; ok {
		return movedError(name, ext)
	}
	return unknownHelp(name)
}

// commandHelp imprime la ayuda del comando path[0] de una extensión (y de su
// subcomando path[1], si el bloque lo describe). prefix va delante del nombre
// en el bloque ("mcp "); exe y base son cómo pedirle sus flags.
func commandHelp(w io.Writer, m *plugin.Manifest, prefix, seeAlso, exe string, base, path []string) error {
	c := m.Command(path[0])
	if c == nil {
		return unknownHelp(strings.TrimSpace(prefix + path[0]))
	}
	full := "kling " + prefix + path[0]
	argv := append(append([]string(nil), base...), path[0])
	if len(path) == 1 {
		flags := ""
		if len(c.Subcommands) == 0 {
			flags = plugin.FlagHelp(exe, argv...)
		}
		plugin.WriteCommand(w, full, prefix, *c, flags, seeAlso)
		return nil
	}
	if !hasSub(c, path[1]) {
		return unknownHelp(strings.TrimSpace(prefix + path[0] + " " + path[1]))
	}
	sub := plugin.Command{Name: path[0] + " " + path[1], Usage: subBlock(c, path[1], strings.Fields(prefix)...)}
	if sub.Usage == "" {
		sub.Usage = plugin.Line(strings.TrimSpace(prefix)+" "+sub.Name, "")
	}
	plugin.WriteCommand(w, full+" "+path[1], prefix, sub, plugin.FlagHelp(exe, append(argv, path[1])...), "kling help "+strings.TrimSpace(prefix+path[0]))
	return nil
}

func unknownHelp(name string) error {
	return &errConCodigo{code: 2, err: &errWithHint{err: fmt.Errorf("unknown command %q", name), hint: "kling help all"}}
}

// helpRequest dice si args es `<palabras> -h`: solo palabras (sin flags ni
// "--") y un -h/--help al final. `kling exec box ls -h` no lo es para
// nosotros: "box" no es un subcomando, y el -h es del ls de dentro.
func helpRequest(cmd string, args []string) ([]string, bool) {
	if os.Getenv(plugin.FlagHelpEnv) != "" || len(args) == 0 || !plugin.IsHelpArg(args[len(args)-1]) || args[len(args)-1] == "help" {
		return nil, false
	}
	words := append([]string{cmd}, args[:len(args)-1]...)
	for _, a := range words[1:] {
		if strings.HasPrefix(a, "-") {
			return nil, false
		}
	}
	// Cada palabra tiene que resolverse en el árbol o en un manifiesto.
	name, rest := resolveAlias(words[0], words[1:])
	if c := coreCommand(name); c != nil {
		return words, len(rest) == 0 || (len(rest) == 1 && hasSub(c, rest[0]))
	}
	p := extensions().Lookup(name)
	if p == nil {
		return nil, false
	}
	if extensions().IsNamespace(name) {
		if len(rest) == 0 {
			return words, true
		}
		c := p.Manifest.Command(rest[0])
		return words, c != nil && (len(rest) == 1 || (len(rest) == 2 && hasSub(c, rest[1])))
	}
	c := p.Manifest.Command(name)
	return words, c != nil && (len(rest) == 0 || (len(rest) == 1 && hasSub(c, rest[0])))
}

// configPath es la ruta de la configuración que se pasa a las extensiones.
func configPath() string { return config.Path() }
