package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// cmdMCP convierte servidores MCP en servicios de kindling.
//
//	kling mcp import <servicio> -image <imagen>
//	kling mcp list
//	kling mcp refresh <servicio>
func cmdMCP(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("uso: kling mcp [import|list|refresh]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "import":
		return mcpImport(rest)
	case "list", "ls":
		return mcpList(rest)
	case "refresh":
		return mcpRefresh(rest)
	case "link":
		return mcpLink(rest)
	case "unlink":
		return mcpUnlink(rest)
	default:
		return fmt.Errorf("subcomando desconocido %q: usa import, list, refresh, link o unlink", sub)
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
	image := fs.String("image", "", "imagen de la que importar (por defecto: el nombre del servicio)")
	mem := fs.Int("mem", 0, "memoria en MiB de la plantilla")
	egress := fs.String("egress", "", "salida de red del servicio: none | internet")
	keep := fs.Bool("keep", false, "no destruir la plantilla al terminar")
	wait := fs.Duration("wait", 45*time.Second, "espera máxima a que el servidor arranque")
	force := fs.Bool("force", false, "reemplazar el servicio si ya existe")
	stateful := fs.Bool("stateful", false, "forzar instancia persistente (por defecto se deduce del catálogo)")
	ephemeral := fs.Bool("ephemeral", false, "forzar máquinas efímeras aunque el análisis diga lo contrario")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling mcp import <servicio> [-image imagen]")
	}
	service := fs.Arg(0)
	img := config_Or(*image, service)

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	c := api.NewClient(cfg.Host(*host))
	tmpl := service + "-import"

	fmt.Printf("Importando %q desde la imagen %q\n\n", service, img)

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

	// 1. plantilla
	fmt.Printf("  1/5  arrancando la plantilla... ")
	mc, err := c.Run(ctx, api.RunRequest{
		Name: tmpl, Image: img, MemMiB: *mem, Egress: *egress,
		Labels: map[string]string{api.LabelService: service},
	})
	if err != nil {
		fmt.Println("✗")
		return fmt.Errorf("no pude arrancar la plantilla: %w", err)
	}
	fmt.Printf("✓ %s en %s\n", mc.ID[:8], mc.IP)

	cleanup := func() {
		if !*keep {
			_ = c.Remove(context.WithoutCancel(ctx), mc.ID)
		}
	}

	// 2. introspección
	fmt.Printf("  2/5  esperando al servidor MCP... ")
	base := "http://" + net.JoinHostPort(mc.IP, strconv.Itoa(8080))
	if err := waitPort(ctx, mc.IP, 8080, *wait); err != nil {
		fmt.Println("✗")
		fmt.Printf("\nEl servidor no abrió el puerto 8080. Mira qué pasó dentro:\n  kling logs %s\n", tmpl)
		cleanup()
		return err
	}
	fmt.Println("✓")

	fmt.Printf("  3/5  preguntando qué sabe hacer... ")
	info, tools, err := introspect(ctx, base)
	if err != nil {
		fmt.Println("✗")
		cleanup()
		return err
	}
	fmt.Printf("✓ %s · %d herramienta(s)\n", info, len(tools))

	// Decisión automática: ¿puede este servicio correr en máquinas efímeras?
	verdict := api.ClassifyTools(tools)
	switch {
	case *stateful:
		verdict = api.StatefulVerdict{Stateful: true, Reason: "forzado con -stateful"}
	case *ephemeral:
		verdict = api.StatefulVerdict{Stateful: false, Reason: "forzado con -ephemeral"}
	}
	modo := "EFÍMERO — una microVM por acción, destruida al terminar"
	if verdict.Stateful {
		modo = "PERSISTENTE — una instancia, congelada al quedar ociosa"
	}
	fmt.Printf("       %s\n", modo)
	fmt.Printf("       porque %s\n", verdict.Reason)

	// 3 y 4. snapshot dorado y catálogo
	fmt.Printf("  4/5  congelando como snapshot dorado... ")
	// La etiqueta viaja con la máquina hasta el snapshot.
	if err := c.SetLabels(ctx, mc.ID, labelsFor(service, verdict.Stateful)); err != nil {
		fmt.Println("✗")
		cleanup()
		return err
	}
	if _, err := c.Commit(ctx, mc.ID, service); err != nil {
		fmt.Println("✗")
		cleanup()
		if strings.Contains(err.Error(), "ya existe") {
			return fmt.Errorf("el servicio %q ya existe; usa -force para reemplazarlo", service)
		}
		return fmt.Errorf("no pude congelarlo: %w", err)
	}
	fmt.Println("✓")

	fmt.Printf("  5/5  guardando el catálogo... ")
	if _, err := c.SetCatalog(ctx, service, tools); err != nil {
		fmt.Println("✗")
		cleanup()
		return err
	}
	fmt.Println("✓")

	cleanup()

	fmt.Printf("\n%q importado con %d herramienta(s):\n", service, len(tools))
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
		fmt.Printf("\nUsará una instancia persistente para no perder lo que acumule. Al quedar\n")
		fmt.Printf("ociosa se congela: deja de gastar CPU y RAM, y vuelve en milisegundos con\n")
		fmt.Printf("su estado intacto.\n")
	}
	fmt.Printf("\nA partir de ahora listar sus capacidades NO despierta la microVM.\n")
	fmt.Printf("Conéctalo:  kling connect -all -install opencode\n")
	return nil
}

func mcpList(args []string) error {
	fs := flag.NewFlagSet("mcp list", flag.ExitOnError)
	host := hostFlag(fs)
	verbose := fs.Bool("v", false, "mostrar cada herramienta")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	snaps, err := api.NewClient(hostOf(*host)).Snapshots(ctx)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Println("Sin servicios. Importa uno:  kling mcp import <nombre> -image <imagen>")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "SERVICIO\tHERRAMIENTAS\tCATÁLOGO\tMEMORIA\tINSTANCIAS")
	total := 0
	for _, s := range snaps {
		n := s.Name
		if svc := s.Service(); svc != "" {
			n = svc
		}
		cat := "sin capturar"
		if s.ToolsAt != nil {
			cat = since(*s.ToolsAt) + " atrás"
		}
		total += len(s.Tools)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\n", n, len(s.Tools), cat, human(s.MemBytes), s.Instances)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	links, _ := api.NewClient(hostOf(*host)).Links(ctx)
	for _, l := range links {
		total += len(l.Tools)
		fmt.Printf("%-12s %-14d %-11s %-9s externo: %s\n",
			l.Service(), len(l.Tools), since(l.CreatedAt)+" atrás", "—", l.URL)
	}
	fmt.Printf("\n%d herramienta(s) en %d servicio(s) (%d microVM, %d externo(s)).\n",
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling mcp refresh <servicio>")
	}
	service := fs.Arg(0)

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	fmt.Printf("Refrescando el catálogo de %q... ", service)
	mc, err := c.Run(ctx, api.RunRequest{
		From: service, Name: service + "-refresh",
		Labels: map[string]string{api.LabelService: service},
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	defer func() { _ = c.Remove(context.WithoutCancel(ctx), mc.ID) }()

	base := "http://" + net.JoinHostPort(mc.IP, "8080")
	if err := waitPort(ctx, mc.IP, 8080, 30*time.Second); err != nil {
		fmt.Println("✗")
		return err
	}
	_, tools, err := introspect(ctx, base)
	if err != nil {
		fmt.Println("✗")
		return err
	}
	if _, err := c.SetCatalog(ctx, service, tools); err != nil {
		fmt.Println("✗")
		return err
	}
	fmt.Printf("✓ %d herramienta(s)\n", len(tools))
	return nil
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
	desc := fs.String("description", "", "para qué sirve")
	var labels labelFlag
	fs.Var(&labels, "label", "etiqueta clave=valor (repetible)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("uso: kling mcp link <nombre> <url>\n" +
			"  ej: kling mcp link engram http://192.168.2.3:9100/mcp")
	}
	name, url := fs.Arg(0), fs.Arg(1)

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	fmt.Printf("Enlazando %q -> %s\n\n", name, url)

	// Se introspecciona igual que a un servicio propio: el catálogo se guarda y
	// a partir de ahí listar capacidades no toca el servidor externo.
	fmt.Printf("  1/2  preguntando qué sabe hacer... ")
	info, tools, err := introspect(ctx, strings.TrimSuffix(url, "/mcp"))
	if err != nil {
		// Puede que la URL ya sea el endpoint completo.
		info, tools, err = introspectAt(ctx, url)
		if err != nil {
			fmt.Println("✗")
			return fmt.Errorf("no pude hablar con %s: %w", url, err)
		}
	}
	fmt.Printf("✓ %s · %d herramienta(s)\n", info, len(tools))

	fmt.Printf("  2/2  registrando... ")
	l, err := c.SetLink(ctx, &api.Link{
		Name: name, URL: url, Description: *desc,
		Labels: labels.merge(name), Tools: tools,
	})
	if err != nil {
		fmt.Println("✗")
		return err
	}
	fmt.Println("✓")

	fmt.Printf("\n%q disponible en el agregador con %d herramienta(s):\n", l.Name, len(l.Tools))
	for _, t := range l.Tools {
		d := t.Description
		if len(d) > 66 {
			d = d[:65] + "…"
		}
		fmt.Printf("  %-28s %s\n", t.Name, d)
	}
	fmt.Printf("\nNo corre en una microVM: se enruta a donde ya vive.\n")
	return nil
}

func mcpUnlink(args []string) error {
	fs := flag.NewFlagSet("mcp unlink", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("uso: kling mcp unlink <nombre>")
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

func waitPort(ctx context.Context, ip string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("el puerto %d no se abrió: %w", port, last)
}

// introspect hace el handshake MCP contra <base>/mcp.
func introspect(ctx context.Context, base string) (string, []api.ToolSpec, error) {
	return introspectAt(ctx, base+"/mcp")
}

// introspectAt lo hace contra una URL completa.
func introspectAt(ctx context.Context, url string) (string, []api.ToolSpec, error) {
	c := &http.Client{Timeout: 45 * time.Second}
	post := func(sid, body string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		return c.Do(req)
	}

	resp, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kling","version":"1"}}}`)
	if err != nil {
		return "", nil, fmt.Errorf("initialize: %w", err)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	var initRes struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&initRes)
	resp.Body.Close()

	name := initRes.Result.ServerInfo.Name
	if name == "" {
		name = "servidor MCP"
	} else if v := initRes.Result.ServerInfo.Version; v != "" {
		name += " v" + v
	}

	// Muchos servidores esperan la notificación `initialized` antes de responder
	// a nada más. Es parte del handshake y omitirla deja a algunos colgados.
	if r, err := post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err == nil {
		r.Body.Close()
	}

	tr, err := post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil {
		return name, nil, fmt.Errorf("tools/list: %w", err)
	}
	defer tr.Body.Close()

	var out struct {
		Result struct {
			Tools []api.ToolSpec `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(tr.Body).Decode(&out); err != nil {
		return name, nil, fmt.Errorf("respuesta ilegible: %w", err)
	}
	if out.Error != nil {
		return name, nil, fmt.Errorf("tools/list: %s", out.Error.Message)
	}
	return name, out.Result.Tools, nil
}

func labelsFor(service string, stateful bool) map[string]string {
	l := map[string]string{api.LabelService: service}
	if stateful {
		l[api.LabelStateful] = "true"
	}
	return l
}

// config_Or evita importar el paquete config solo por un valor por defecto.
func config_Or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
