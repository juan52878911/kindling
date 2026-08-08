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
	default:
		return fmt.Errorf("subcomando desconocido %q: usa import, list o refresh", sub)
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
	stateful := fs.Bool("stateful", false, "el servicio acumula estado: usa instancia persistente en vez de efímera")
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
		Labels: labelsFor(service, *stateful),
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

	// 3 y 4. snapshot dorado y catálogo
	fmt.Printf("  4/5  congelando como snapshot dorado... ")
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

	if *stateful {
		fmt.Printf("\nMarcado como CON ESTADO: usará una instancia persistente, congelada al\n")
		fmt.Printf("quedar ociosa, para no perder lo que acumule entre llamadas.\n")
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
	fmt.Printf("\n%d herramienta(s) en %d servicio(s).\n", total, len(snaps))

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

// introspect hace el handshake MCP y devuelve el servidor y sus herramientas.
func introspect(ctx context.Context, base string) (string, []api.ToolSpec, error) {
	c := &http.Client{Timeout: 45 * time.Second}
	post := func(sid, body string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", strings.NewReader(body))
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
