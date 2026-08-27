package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/config"
)

// cmdMCP convierte servidores MCP en servicios de kindling.
//
//	kling mcp import <servicio> -image <imagen>
//	kling mcp list
//	kling mcp refresh <servicio>
func cmdMCP(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling mcp [import|verify|list|refresh|health|heal]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "import":
		return mcpImport(rest)
	case "verify":
		return mcpVerify(rest)
	case "list", "ls":
		return mcpList(rest)
	case "refresh":
		return mcpRefresh(rest)
	case "health":
		return mcpHealth(rest)
	case "heal":
		return mcpHeal(rest)
	case "link":
		return mcpLink(rest)
	case "unlink":
		return mcpUnlink(rest)
	default:
		return fmt.Errorf("unknown subcommand %q: use import, verify, list, refresh, health, heal, link, or unlink", sub)
	}
}

// mcpImport hace el ciclo completo de conversión de un servidor MCP.
//
//  1. arranca una máquina plantilla desde la imagen
//  2. INTROSPECCIÓN: le pregunta qué sabe hacer (initialize + tools/list)
//  3. la congela como snapshot dorado
//  4. guarda el catálogo junto al snapshot
//  5. destruye la plantilla
//
// El paso 4 es el que hace que a partir de aquí nadie tenga que despertar la
// microVM para saber qué herramientas ofrece.
func mcpImport(args []string) error {
	fs := flag.NewFlagSet("mcp import", flag.ExitOnError)
	host := hostFlag(fs)
	image := fs.String("image", "", "image to import from (default: the service name)")
	mem := fs.Int("mem", 0, "template memory in MiB")
	// Sin esto solo se podía subir la memoria, y hay servicios cuyo cuello de
	// botella es la CPU: un analizador estático pasa por cada fichero, y con un
	// solo vCPU un escaneo se acerca al plazo del cliente MCP. Como la memoria,
	// queda GRABADO en el snapshot dorado y no se puede cambiar después sin
	// reimportar — por eso el flag va aquí y no en el arranque.
	cpus := fs.Int("cpus", 0, "template vCPUs")
	// El techo de CPU también se graba en el snapshot dorado (ver api.Snapshot):
	// las instancias nacen de él, y sin fijarlo aquí toda restauración caía al 50 %
	// del daemon. En Mac ese estrangulamiento a media vCPU dobla el arranque en frío
	// de node (16 s → 6.9 s al 100 %), así que subirlo es la palanca directa allí.
	cpuPct := fs.Int("cpu-pct", 0, "CPU cap in % of one core for the template (0 = daemon default)")
	cpu := fs.Int("cpu", 0, "deprecated alias of -cpu-pct")
	egress := fs.String("egress", "", "service network egress: none | internet | allowlist")
	allow := fs.String("allow", "", "domains allowed with -egress allowlist (comma-separated)")
	var volumes volumeFlag
	fs.Var(&volumes, "volume", "volume to mount: name[:/mountpoint][:ro] (repeatable)")
	mount := fs.String("mount", "", "where to mount the volume (default /data; only with one)")
	volRO := fs.Bool("volume-ro", false, "mount it read-only: shareable across services")
	keep := fs.Bool("keep", false, "don't destroy the template when done")
	wait := fs.Duration("wait", 45*time.Second, "maximum wait for the server to start")
	force := fs.Bool("force", false, "replace the service if it already exists")
	stateful := fs.Bool("stateful", false, "force a persistent instance (default: inferred from the catalog)")
	ephemeral := fs.Bool("ephemeral", false, "force ephemeral machines even if the analysis says otherwise")
	allowRuntimeInstall := fs.Bool("allow-runtime-install", false,
		"import even if the server installs dependencies at runtime (not recommended: bake them into the image)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	importVols, err := volumeSet(volumes, *mount, *volRO)
	if err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling mcp import <service> [-image image]")
	}
	service := fs.Arg(0)
	img := config.Or(*image, service)

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	c := api.NewClient(cfg.Host(*host))
	tmpl := service + "-import"

	fmt.Printf("Importing %q from image %q\n\n", service, img)

	// Reimportar es lo normal cuando cambia la versión del servidor MCP, así que
	// hay que poder reemplazar: primero se retiran sus instancias, que son las
	// que impiden borrar el snapshot.
	if *force {
		machines, _ := c.List(ctx)
		for _, m := range machines {
			if m.From == service || m.Service() == service {
				_ = c.Remove(ctx, m.ID)
			}
		}
		_ = c.RemoveSnapshot(ctx, service)
	}

	// Capacidades de la imagen: detectadas al construirla del árbol de deps. Si
	// declara que necesita internet y el usuario no forzó egress, se pone solo —
	// que un servicio de navegador o de API remota no arranque por un flag
	// olvidado es un mal por defecto. También avisa de módulos nativos.
	egr := config.Or(*egress, cfg.Defaults.Egress)
	// Dominios permitidos: lo que ponga -allow manda; si no, la semilla que el
	// build extrajo del árbol npm (editable, no exhaustiva) rellena el hueco.
	allowDomains := splitDomains(*allow)
	if caps, cerr := c.ImageCapabilities(ctx, img); cerr == nil && caps != nil {
		if egr == "" && caps.Egress == "allowlist" {
			egr = "allowlist"
			fmt.Printf("  capabilities: the service declares allowlist -> egress=allowlist (automatic)\n")
		}
		if egr == "" && (caps.Egress == "internet" || caps.Browser) {
			egr = "internet"
			fmt.Printf("  capabilities: the service needs internet -> egress=internet (automatic)\n")
		}
		if egr == "allowlist" && len(allowDomains) == 0 && len(caps.AllowDomains) > 0 {
			allowDomains = caps.AllowDomains
			fmt.Printf("  capabilities: domain seed from the build (%d); edit it with -allow if any are missing\n",
				len(caps.AllowDomains))
		}
		if len(caps.System) > 0 {
			// Ya horneados en la imagen por el build; solo se informa de qué trae.
			fmt.Printf("  capabilities: system binaries in the image (%s)\n",
				strings.Join(caps.System, ", "))
		}
		if len(caps.Native) > 0 {
			fmt.Printf("  warning: native modules detected (%s); if the server fails when using them, rebuild installing them with their scripts\n",
				strings.Join(caps.Native, ", "))
		}
		// ERROR, no aviso: estos nativos quedaron sin binario. Antes la imagen se
		// importaba "bien" y petaba en la primera tool que tocara sharp/canvas/…
		// Se aborta ANTES de arrancar la plantilla: no tiene sentido gastar el
		// ciclo de import en un servicio que se sabe roto.
		if len(caps.NativeMissing) > 0 {
			return fmt.Errorf("native modules without a binary: %s\n"+
				"The image was built with --ignore-scripts (it doesn't compile on the host) and these modules\n"+
				"didn't ship a prebuilt binary in the package, nor a platform package that provided one. The server\n"+
				"would start, but the first tool that uses them would fail at runtime.\n"+
				"Fix it by rebuilding with a version that publishes platform binaries\n"+
				"(e.g. sharp>=0.33 with its optionalDependencies @img/sharp-*) or by adding the matching\n"+
				"platform package to the image's npm dependencies.",
				strings.Join(caps.NativeMissing, ", "))
		}
	}
	if egr == "" {
		egr = "none"
	}
	if egr == "allowlist" {
		if len(allowDomains) == 0 {
			// Sin dominios el modo cierra en falso: solo DNS, cero salida útil. Es
			// legítimo (deny-all con DNS) pero casi siempre un despiste, así que se
			// avisa en vez de fallar.
			fmt.Printf("  warning: allowlist WITHOUT domains; the service won't be able to reach anywhere. Add -allow dom1,dom2\n")
		} else {
			fmt.Printf("  allowlist: %s\n", strings.Join(allowDomains, ", "))
		}
	}

	// 1. plantilla
	fmt.Printf("  1/5  starting the template... ")
	mc, err := c.Run(ctx, api.RunRequest{
		Name:  tmpl,
		Image: img,
		// Los valores por defecto se aplican IGUAL que en `kling run`. No
		// hacerlo era un fallo caro y silencioso: la memoria QUEDA GRABADA en
		// el snapshot dorado, así que un servicio importado se quedaba con los
		// 256 MiB del daemon aunque su dueño hubiera puesto defaults.mem_mib a
		// 1024 — que es justo lo que se sube para aguantar varias sesiones,
		// porque cada una arranca su propio proceso del servidor MCP dentro del
		// invitado.
		MemMiB: config.Or(*mem, cfg.Defaults.MemMiB, 256),
		VCPUs:  config.Or(*cpus, cfg.Defaults.VCPUs, 1),
		// Techo de CPU: se graba en el snapshot (Commit copia mc.CPUPct) para que la
		// restauración no caiga al 50 % del daemon. 0 = deja decidir al daemon.
		CPUPct: config.Or(resolveCPUPct(fs, *cpuPct, *cpu), cfg.Defaults.CPUPct),
		Egress: egr,
		// Se graban en el snapshot dorado: las instancias nacen de él y sin esto
		// despertarían con la lista vacía. Solo se usan si egr == "allowlist".
		AllowDomains: allowDomains,
		// El volumen se decide AQUÍ y no después: Firecracker no deja añadir
		// discos a una VM restaurada, así que el dispositivo tiene que estar
		// presente cuando se congela el snapshot dorado o no lo estará nunca.
		Volumes: importVols,
		Labels:  map[string]string{api.LabelService: service},
	})
	if err != nil {
		fmt.Println("✗")
		return fmt.Errorf("couldn't start the template: %w", err)
	}
	fmt.Printf("✓ %s en %s\n", mc.ID[:8], mc.IP)

	cleanup := func() {
		if !*keep {
			_ = c.Remove(context.WithoutCancel(ctx), mc.ID)
		}
	}

	// 2. introspección
	fmt.Printf("  2/5  waiting for the MCP server... ")
	if err := waitGuest(ctx, c, mc.ID, *wait); err != nil {
		fmt.Println("✗")
		fmt.Printf("\nThe server didn't open port 8080. Check what happened inside:\n  kling logs %s\n", tmpl)
		cleanup()
		return err
	}
	fmt.Println("✓")

	fmt.Printf("  3/5  asking what it can do... ")
	info, tools, err := introspectWith(guestPost(ctx, c, mc.ID))
	if err != nil {
		fmt.Println("✗")
		cleanup()
		return err
	}
	fmt.Printf("✓ %s · %d tool(s)\n", info, len(tools))

	// GUARDIÁN: que el servidor no se instale nada en caliente.
	//
	// La microVM corre aislada (egress:none), igual que en producción, así que un
	// servidor que intente `pip install`/`npx -y` al arrancar ya ha fracasado para
	// cuando llegamos aquí, y su huella está en la consola. Se comprueba ANTES del
	// commit: no queremos congelar un snapshot dorado de un servicio roto. Ver
	// verify.go.
	fmt.Printf("       checking that it doesn't install at runtime... ")
	if logtxt, lerr := c.Logs(ctx, mc.ID, 0); lerr == nil {
		if hits := detectRuntimeInstall(logtxt); len(hits) > 0 {
			fmt.Println("✗")
			msg := installFindingsMsg(service, hits)
			if !*allowRuntimeInstall {
				cleanup()
				return fmt.Errorf("%s", msg)
			}
			fmt.Println(msg)
			fmt.Println("       --allow-runtime-install: importing anyway")
		} else {
			// El escaneo de consola caza a quien instala al arrancar. Los que solo
			// instalan al USAR una herramienta (semgrep) no se disparan aquí: para
			// eso está `kling mcp verify`, que ejerce las herramientas.
			fmt.Println("✓ (for the deep check: kling mcp verify " + service + ")")
		}
	} else {
		// Sin consola no se puede afirmar que esté limpio, pero tampoco es motivo
		// para abortar: se deja constancia y se sigue.
		fmt.Printf("(no log: %v)\n", lerr)
	}

	// AUTO-RESET POST-CATALOG. Sin esto, el snapshot dorado se congela con el
	// servidor en estado post-handshake: sesiones abiertas, procesos hijos
	// hablando por pipes. Al restaurar, la microVM queda con un proceso zombie
	// que rechaza nuevos handshakes (HTTP 400 "Server already initialized" o
	// 406).
	//
	// Hay dos casos, según el modo de la imagen:
	//
	//   stdio + bridge (kling-bridge): el bridge expone POST /reset, que cierra
	//   TODAS las sesiones y mata los procesos hijo. Instantáneo.
	//
	//   HTTP nativo (mcp-server-X streamableHttp): el wrapper del entrypoint
	//   (ver scripts/80-mcp-image.sh) hace un kill -KILL del servidor tras
	//   KLING_HTTP_RESET_AFTER segundos y lo re-arranca. Necesita tiempo.
	//
	// Probamos /reset primero. Si responde 204, es stdio y ya está. Si da 404,
	// es HTTP nativo: esperamos el auto-reset del wrapper.
	fmt.Printf("       resetting post-handshake state... ")
	gresp, err := c.Guest(ctx, mc.ID, api.GuestRequest{
		Path:   "/reset",
		Method: "POST",
	})
	if err == nil && gresp.Status == 204 {
		fmt.Println("✓ (bridge)")
	} else {
		// HTTP nativo: el wrapper hace kill -KILL del servidor tras 30s y lo
		// re-arranca. Le damos un poco más de margen y verificamos que el
		// puerto vuelve a abrir antes del commit.
		resetAfter := 35 * time.Second
		if *wait < resetAfter {
			resetAfter = *wait + 5*time.Second
		}
		fmt.Printf("waiting %s for the native HTTP server auto-reset... ", resetAfter)
		select {
		case <-time.After(resetAfter):
			fmt.Println("✓ (native HTTP)")
		case <-ctx.Done():
			fmt.Println("✗")
			cleanup()
			return ctx.Err()
		}
		fmt.Printf("       verifying the server responds... ")
		if err := waitGuest(ctx, c, mc.ID, 30*time.Second); err != nil {
			fmt.Println("✗")
			fmt.Printf("\nThe server didn't reopen port 8080 after the reset. Check what happened:\n  kling logs %s\n", tmpl)
			cleanup()
			return err
		}
		fmt.Println("✓")
	}

	// Decisión automática: ¿puede este servicio correr en máquinas efímeras?
	verdict := api.ClassifyTools(tools)
	switch {
	case *stateful:
		verdict = api.StatefulVerdict{Stateful: true, Reason: "forced with -stateful"}
	case *ephemeral:
		verdict = api.StatefulVerdict{Stateful: false, Reason: "forced with -ephemeral"}
	}
	modo := "EPHEMERAL — one microVM per action, destroyed when done"
	if verdict.Stateful {
		modo = "PERSISTENT — one instance, frozen when idle"
	}
	fmt.Printf("       %s\n", modo)
	fmt.Printf("       because %s\n", verdict.Reason)

	// 3 y 4. snapshot dorado y catálogo
	fmt.Printf("  4/5  freezing as golden snapshot... ")
	// La etiqueta viaja con la máquina hasta el snapshot.
	if err := c.SetLabels(ctx, mc.ID, labelsFor(service, verdict.Stateful)); err != nil {
		fmt.Println("✗")
		cleanup()
		return err
	}
	if _, err := c.Commit(ctx, mc.ID, service, false); err != nil {
		fmt.Println("✗")
		cleanup()
		if strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("service %q already exists; use -force to replace it", service)
		}
		return fmt.Errorf("couldn't freeze it: %w", err)
	}
	fmt.Println("✓")

	fmt.Printf("  5/5  saving the catalog... ")
	if _, err := c.SetCatalog(ctx, service, tools); err != nil {
		fmt.Println("✗")
		cleanup()
		return err
	}
	fmt.Println("✓")

	cleanup()

	fmt.Printf("\n%q imported with %d tool(s):\n", service, len(tools))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	for _, t := range tools {
		d := t.Description
		if len(d) > 68 {
			d = d[:67] + "…"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", t.Name, d)
	}
	_ = tw.Flush()

	if verdict.Stateful {
		fmt.Printf("\nIt will use a persistent instance so it doesn't lose what it accumulates. When idle\n")
		fmt.Printf("it freezes: it stops spending CPU and RAM, and comes back in milliseconds with\n")
		fmt.Printf("its state intact.\n")
	}
	if egr == "allowlist" {
		if len(allowDomains) > 0 {
			fmt.Printf("\nRestricted egress (allowlist) to: %s\n", strings.Join(allowDomains, ", "))
		} else {
			fmt.Printf("\nRestricted egress (allowlist) WITHOUT domains: it won't be able to reach anywhere.\n")
		}
		fmt.Printf("Change the list by reimporting with -allow dom1,dom2.\n")
	}
	fmt.Printf("\nFrom now on, listing its capabilities does NOT wake the microVM.\n")
	fmt.Printf("Connect it:  kling connect -all -install opencode\n")
	return nil
}

func mcpList(args []string) error {
	fs := flag.NewFlagSet("mcp list", flag.ExitOnError)
	host := hostFlag(fs)
	verbose := fs.Bool("v", false, "show each tool")
	asJSON := fs.Bool("json", false, "JSON output (services + external, with tools)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		// Los links (servicios externos) también son servicios MCP: se emiten
		// aparte para que un consumidor distinga microVM de puente externo.
		links, _ := c.Links(ctx)
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"services": snaps,
			"external": links,
		})
	}
	if len(snaps) == 0 {
		fmt.Println("No services. Import one:  kling mcp import <name> -image <image>")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tTOOLS\tCATALOG\tHEALTH\tMEMORY\tINSTANCES")
	total, unprobed := 0, 0
	for _, s := range snaps {
		n := s.Name
		if svc := s.Service(); svc != "" {
			n = svc
		}
		cat := "not captured"
		if s.ToolsAt != nil {
			cat = since(*s.ToolsAt) + " ago"
		}
		if s.Health == "" {
			unprobed++
		}
		total += len(s.Tools)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\n",
			n, len(s.Tools), cat, healthCell(s), human(s.MemBytes), s.Instances)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	links, _ := c.Links(ctx)
	// "not probed" no es un dato de salud, es su ausencia — y una columna llena
	// de ausencias sin decir cómo llenarla es como se pasan 26 horas de caída
	// sin que nadie sondee. El sondeo no ocurre aquí: cada sondeo despierta una
	// microVM y `ls` debe seguir siendo instantáneo.
	if unprobed > 0 {
		fmt.Printf("\nHealth has never been probed for %d service(s). Probe them:  kling mcp health\n", unprobed)
	}
	for _, l := range links {
		total += len(l.Tools)
		fmt.Printf("%-12s %-14d %-11s %-13s %-9s external: %s\n",
			l.Service(), len(l.Tools), since(l.CreatedAt)+" ago", "—", "—", l.URL)
	}
	fmt.Printf("\n%d tool(s) across %d service(s) (%d microVM, %d external).\n",
		total, len(snaps)+len(links), len(snaps), len(links))

	if *verbose {
		for _, s := range snaps {
			if len(s.Tools) == 0 {
				continue
			}
			fmt.Printf("\n%s:\n", s.Name)
			for _, t := range s.Tools {
				d := t.Description
				if len(d) > 70 {
					d = d[:69] + "…"
				}
				fmt.Printf("  %-32s %s\n", t.Name, d)
			}
		}
	}
	return nil
}

// mcpRefresh vuelve a capturar el catálogo de un servicio ya importado.
func mcpRefresh(args []string) error {
	fs := flag.NewFlagSet("mcp refresh", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling mcp refresh <service>")
	}
	service := fs.Arg(0)

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	fmt.Printf("Refreshing the catalog for %q... ", service)
	mc, err := c.Run(ctx, api.RunRequest{
		From: service, Name: service + "-refresh",
		Labels: map[string]string{api.LabelService: service},
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	defer func() { _ = c.Remove(context.WithoutCancel(ctx), mc.ID) }()

	if err := waitGuest(ctx, c, mc.ID, 30*time.Second); err != nil {
		fmt.Println("✗")
		return err
	}
	_, tools, err := introspectWith(guestPost(ctx, c, mc.ID))
	if err != nil {
		fmt.Println("✗")
		return err
	}
	if _, err := c.SetCatalog(ctx, service, tools); err != nil {
		fmt.Println("✗")
		return err
	}
	fmt.Printf("✓ %d tool(s)\n", len(tools))
	return nil
}

// mcpHealth sondea la salud de uno o de todos los servicios importados.
//
//	kling mcp health            sondea todos
//	kling mcp health <servicio> sondea uno
//
// Para cada servicio arranca una microVM efímera del snapshot dorado, le pide
// tools/list y la destruye, marcándolo healthy/unhealthy en su meta. Reutiliza el
// MISMO camino de arranque que el modo efímero del gateway (Run desde el snapshot
// + tools/list), no inventa uno nuevo.
//
// TODO(P1-4): sondeo periódico automático (una vez por hora). El scheduler
// debería vivir en un proceso de vida larga —el daemon o el gateway— y llamar a
// este mismo camino de sondeo. Se deja fuera de esta pasada a propósito: el
// sondeo manual ya cubre la operación y el CI, y el scheduler es aditivo.
func mcpHealth(args []string) error {
	fs := flag.NewFlagSet("mcp health", flag.ExitOnError)
	host := hostFlag(fs)
	wait := fs.Duration("wait", 45*time.Second, "maximum wait for the server to start")
	profundo := fs.Bool("deep", true, "also call one real tool, not just tools/list")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	// Qué sondear: el servicio indicado, o todos los snapshots si no se da ninguno.
	var targets []string
	if fs.NArg() >= 1 {
		targets = []string{fs.Arg(0)}
	} else {
		snaps, err := c.Snapshots(ctx)
		if err != nil {
			return err
		}
		for _, s := range snaps {
			n := s.Name
			if svc := s.Service(); svc != "" {
				n = svc
			}
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		fmt.Println("No services to probe. Import one:  kling mcp import <name> -image <image>")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	var enfermos int
	for _, svc := range targets {
		probeErr := probeHealth(ctx, c, svc, *wait, *profundo)
		// El veredicto se persiste aunque el servicio esté roto: "enferma" es un
		// dato tan útil como "sana", y es justo el que queremos ver en mcp list.
		if _, err := c.SetHealth(ctx, svc, probeErr == nil, errMsg(probeErr)); err != nil {
			fmt.Fprintf(tw, "  %s\t✗ couldn't record health: %v\n", svc, err)
			continue
		}
		if probeErr != nil {
			enfermos++
			fmt.Fprintf(tw, "  %s\t✗ unhealthy: %v\n", svc, probeErr)
		} else {
			fmt.Fprintf(tw, "  %s\t✓ healthy\n", svc)
		}
	}
	_ = tw.Flush()

	if enfermos > 0 {
		return fmt.Errorf("%d of %d service(s) didn't respond to the probe", enfermos, len(targets))
	}
	return nil
}

// probeHealth arranca una instancia efímera del snapshot, le pide tools/list y la
// destruye. Devuelve nil si el servicio contestó. Es el mismo ciclo que hace el
// gateway en modo efímero, expresado con las utilidades del CLI.
func probeHealth(ctx context.Context, c *api.Client, service string, wait time.Duration, profundo bool) error {
	mc, err := c.Run(ctx, api.RunRequest{
		From: service, Name: service + "-health",
		Labels: map[string]string{api.LabelService: service, "health": "true"},
		// Red de seguridad: si el CLI muriera antes de destruirla, el daemon la
		// congela sola en vez de dejarla corriendo para siempre.
		TTLSeconds: 120,
	})
	if err != nil {
		return fmt.Errorf("didn't start (%w)", err)
	}
	defer func() { _ = c.Remove(context.WithoutCancel(ctx), mc.ID) }()

	if err := waitGuest(ctx, c, mc.ID, wait); err != nil {
		return fmt.Errorf("didn't open the MCP port (%w)", err)
	}
	post := guestPost(ctx, c, mc.ID)
	_, tools, err := introspectWith(post)
	if err != nil {
		return fmt.Errorf("didn't respond to tools/list (%w)", err)
	}
	if !profundo {
		return nil
	}
	// Y ahora la parte que SI puede fallar. Ver prueba.go: tools/list lo
	// contesta el servidor sin tocar lo que de verdad usa, asi que hasta aqui
	// un servicio de navegador con el navegador roto sale impecable.
	herramienta, args, que, hay := pruebaAplicable(tools)
	if !hay {
		return nil
	}
	if err := ejercitar(post, herramienta, args); err != nil {
		return fmt.Errorf("answers tools/list but doesn't work: checking that %s failed: %w", que, err)
	}
	return nil
}

// healthCell resume el estado de salud de un snapshot para `mcp list`.
func healthCell(s *api.Snapshot) string {
	switch s.Health {
	case "healthy":
		if s.HealthAt != nil {
			return "healthy (" + since(*s.HealthAt) + ")"
		}
		return "healthy"
	case "unhealthy":
		if s.HealthAt != nil {
			return "unhealthy (" + since(*s.HealthAt) + ")"
		}
		return "unhealthy"
	default:
		return "not probed"
	}
}

// errMsg devuelve el texto de un error, o "" si es nil. Sirve para persistir el
// motivo de un sondeo fallido sin encadenar comprobaciones de nil en el llamador.
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// mcpLink registra un servidor MCP EXTERNO en el agregador.
//
// No corre en una microVM: sigue viviendo donde su dueño lo tenga. Es la vía para
// traer capacidades que no tiene sentido meter en una máquina efímera —memoria,
// sobre todo—: un sitio donde todas las herramientas puedan guardar y leer, sin
// que kindling implemente almacenamiento.
func mcpLink(args []string) error {
	fs := flag.NewFlagSet("mcp link", flag.ExitOnError)
	host := hostFlag(fs)
	desc := fs.String("description", "", "what it's for")
	var labels labelFlag
	fs.Var(&labels, "label", "key=value label (repeatable)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling mcp link <name> <url>\n" +
			"  e.g.: kling mcp link engram http://192.168.2.3:9100/mcp")
	}
	name, url := fs.Arg(0), fs.Arg(1)

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	fmt.Printf("Linking %q -> %s\n\n", name, url)

	// Se introspecciona igual que a un servicio propio: el catálogo se guarda y
	// a partir de ahí listar capacidades no toca el servidor externo.
	fmt.Printf("  1/2  asking what it can do... ")
	info, tools, err := introspectAt(ctx, strings.TrimSuffix(url, "/mcp")+"/mcp")
	if err != nil {
		// Puede que la URL ya sea el endpoint completo.
		info, tools, err = introspectAt(ctx, url)
		if err != nil {
			fmt.Println("✗")
			return fmt.Errorf("couldn't talk to %s: %w", url, err)
		}
	}
	fmt.Printf("✓ %s · %d tool(s)\n", info, len(tools))

	fmt.Printf("  2/2  registering... ")
	l, err := c.SetLink(ctx, &api.Link{
		Name: name, URL: url, Description: *desc,
		Labels: labels.merge(name), Tools: tools,
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	fmt.Println("✓")

	fmt.Printf("\n%q available in the aggregator with %d tool(s):\n", l.Name, len(l.Tools))
	for _, t := range l.Tools {
		d := t.Description
		if len(d) > 66 {
			d = d[:65] + "…"
		}
		fmt.Printf("  %-28s %s\n", t.Name, d)
	}
	fmt.Printf("\nIt doesn't run in a microVM: it's routed to wherever it already lives.\n")
	return nil
}

func mcpUnlink(args []string) error {
	fs := flag.NewFlagSet("mcp unlink", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling mcp unlink <name>")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	if err := api.NewClient(hostOf(*host)).RemoveLink(ctx, fs.Arg(0)); err != nil {
		return err
	}
	fmt.Println(fs.Arg(0))
	return nil
}

// ── utilidades ────────────────────────────────────────────────────────────────

// waitGuest espera a que el servidor de dentro de la microVM abra su puerto.
// La espera ocurre en el daemon: las IP de los invitados no son alcanzables
// desde un CLI remoto, y sondearlas desde aquí falla siempre por SSH.
func waitGuest(ctx context.Context, c *api.Client, ref string, timeout time.Duration) error {
	_, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: 8080, WaitMS: int(timeout / time.Millisecond), ProbeOnly: true,
	})
	return err
}

// poster manda una petición MCP y devuelve el id de sesión que asignó el
// servidor y su respuesta. Hay dos formas de llegar al servidor —directamente,
// si es externo, o por el daemon, si vive dentro de una microVM— y la
// introspección no necesita saber cuál es.
type poster func(sid, body string) (string, []byte, error)

// directPost habla con una URL alcanzable desde aquí: servidores enlazados.
func directPost(ctx context.Context, url string) poster {
	// Sin Timeout global: acotarlo aquí acota el ARRANQUE del servidor MCP, que
	// es trabajo legítimo y muy variable —un servidor de node con semgrep
	// dentro tarda bastante más que un eco—. Lo que sí se acota es la espera a
	// las cabeceras, que separa "está pensando" de "no hay nadie", y por encima
	// manda el contexto de quien llama (-wait).
	c := &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: 4 * time.Minute,
	}}
	return func(sid, body string) (string, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			return "", nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", api.AcceptMCP)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := c.Do(req)
		if err != nil {
			return "", nil, err
		}
		defer resp.Body.Close()
		out, err := api.LeerCuerpo(resp.Body, 8<<20)
		return resp.Header.Get("Mcp-Session-Id"), out, err
	}
}

// guestPost habla con el servidor de dentro de una microVM, pasando por el
// daemon, que es quien tiene ruta hasta él.
func guestPost(ctx context.Context, c *api.Client, ref string) poster {
	return func(sid, body string) (string, []byte, error) {
		req := api.GuestRequest{Port: 8080, Path: "/mcp", Body: body}
		if sid != "" {
			req.Headers = map[string]string{"Mcp-Session-Id": sid}
		}
		resp, err := c.Guest(ctx, ref, req)
		if err != nil {
			return "", nil, err
		}
		return resp.Headers["Mcp-Session-Id"], []byte(resp.Body), nil
	}
}

// introspectAt hace el handshake MCP contra una URL alcanzable desde aquí.
func introspectAt(ctx context.Context, url string) (string, []api.ToolSpec, error) {
	return introspectWith(directPost(ctx, url))
}

// introspectWith hace el handshake sin saber por dónde viajan las peticiones.
func introspectWith(post poster) (string, []api.ToolSpec, error) {
	sid, raw, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kling","version":"1"}}}`)
	if err != nil {
		return "", nil, fmt.Errorf("initialize: %w", err)
	}
	var initRes struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	_ = json.Unmarshal(api.MCPPayload(raw), &initRes)

	name := initRes.Result.ServerInfo.Name
	if name == "" {
		name = "MCP server"
	} else if v := initRes.Result.ServerInfo.Version; v != "" {
		name += " v" + v
	}

	// Muchos servidores esperan la notificación `initialized` antes de responder
	// a nada más. Es parte del handshake y omitirla deja a algunos colgados.
	_, _, _ = post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	_, raw, err = post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil {
		return name, nil, fmt.Errorf("tools/list: %w", err)
	}

	var out struct {
		Result struct {
			Tools []api.ToolSpec `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(api.MCPPayload(raw), &out); err != nil {
		// Enseñar lo que llegó, no solo que no se pudo parsear.
		//
		// Cuando el servidor MCP muere al arrancar —le faltan argumentos, el
		// paquete de npm está roto— el puente contesta con un error en texto
		// plano, y "respuesta ilegible: invalid character 'l'" no dice
		// absolutamente nada sobre la causa. El cuerpo sí.
		body := strings.TrimSpace(string(api.MCPPayload(raw)))
		if len(body) > 300 {
			body = body[:300] + "…"
		}
		if body == "" {
			body = "(empty response)"
		}
		return name, nil, fmt.Errorf("couldn't understand the response to tools/list (%w).\n"+
			"The server replied: %s", err, body)
	}
	if out.Error != nil {
		return name, nil, fmt.Errorf("tools/list: %s", out.Error.Message)
	}
	return name, out.Result.Tools, nil
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

func labelsFor(service string, stateful bool) map[string]string {
	l := map[string]string{api.LabelService: service}
	if stateful {
		l[api.LabelStateful] = "true"
	}
	return l
}
