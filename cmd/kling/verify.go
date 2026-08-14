package main

// Guardián contra la instalación EN CALIENTE.
//
// El trato de kindling es que todo lo que un servidor MCP necesita se hornea en
// la imagen compartida (vda, de solo lectura) al construirla, y en tiempo de
// ejecución la microVM solo lo LEE. Un servidor que en su lugar hace `pip
// install` o `npx -y` al arrancar —o al primer uso de una herramienta— rompe ese
// trato de la peor manera: instala en el overlay efímero (que muere con la
// sesión, así que lo reinstala una y otra vez), y como la microVM corre sin
// salida a internet, el intento además FALLA y se lleva por delante la memoria.
// Le pasó a semgrep: el primer escaneo intentaba `pip3 install semgrep`, fallaba
// con `externally-managed-environment` y el OOM-killer del invitado mataba
// procesos.
//
// La introspección (initialize + tools/list) NO basta para detectarlo: el
// handshake MCP responde igual aunque el binario de debajo falte, y hay
// servidores —semgrep entre ellos— que solo intentan instalar al EJECUTAR una
// herramienta. Por eso el guardián ejerce cada herramienta con argumentos
// mínimos y luego vigila la consola durante una ventana: el intento de instalar
// deja su huella ahí, aunque tarde en llegar.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/config"
)

// installSig es una huella de "instalando algo en caliente" en la consola o en
// la respuesta de una herramienta.
type installSig struct {
	needle string // subcadena a buscar, en minúsculas
	why    string // qué significa, para el diagnóstico
}

// installSigs son las huellas que delatan una instalación en tiempo de ejecución.
//
// Tres familias: el COMANDO de instalar (pip/npm/…), el HOST de un registro de
// paquetes (alcanzarlo en runtime es descargar), y el FRACASO que ese intento
// produce sin red. Basta una para levantar la bandera; juntas dan un diagnóstico
// que se lee solo.
var installSigs = []installSig{
	// gestores de paquetes lanzados en runtime
	{"pip install", "pip installing Python packages at runtime"},
	{"pip3 install", "pip installing Python packages at runtime"},
	{"pip download", "pip downloading packages at runtime"},
	{"uv pip install", "uv installing Python packages at runtime"},
	{"pipx install", "pipx installing a tool at runtime"},
	{"npm install", "npm installing Node packages at runtime"},
	{"npm i ", "npm installing Node packages at runtime"},
	{"npx -y", "npx downloading a package at runtime"},
	{"pnpm add", "pnpm installing packages at runtime"},
	{"pnpm dlx", "pnpm downloading a package at runtime"},
	{"yarn add", "yarn installing packages at runtime"},
	{"apk add", "apk installing system packages at runtime"},
	{"apt-get install", "apt installing system packages at runtime"},
	{"apt install", "apt installing system packages at runtime"},
	{"cargo install", "cargo compiling/installing at runtime"},
	{"go install", "go installing a binary at runtime"},
	{"gem install", "gem installing at runtime"},
	// hosts de registros de paquetes: alcanzarlos en runtime es descargar
	{"registry.npmjs.org", "reaches the npm registry at runtime"},
	{"pypi.org", "reaches PyPI at runtime"},
	{"files.pythonhosted.org", "downloads PyPI wheels at runtime"},
	{"registry.yarnpkg.com", "reaches the yarn registry at runtime"},
	// el servidor mismo informa de que le falta o instala una dependencia
	{"error installing", "the server reported a failure installing a dependency"},
	{"failed to install", "the server reported a failure installing a dependency"},
	{"is not installed", "the server says its tool isn't installed"},
	{"please install", "the server asks to install a missing dependency"},
	// fracasos que prueban que lo intentó y no pudo (sin red)
	{"externally-managed-environment", "tried pip install and the system rejected it (PEP 668)"},
	{"could not resolve host", "a downloader couldn't resolve a host (no network in the microVM)"},
	{"enotfound", "npm/node couldn't resolve a download host (no network in the microVM)"},
	{"temporary failure in name resolution", "a download couldn't resolve DNS (no network in the microVM)"},
	{"network is unreachable", "a download couldn't reach the network (the microVM runs isolated)"},
}

// detectRuntimeInstall busca en el texto (consola + respuestas de herramientas)
// huellas de instalación en caliente y devuelve una línea de evidencia por cada
// huella DISTINTA encontrada. Vacío = limpio.
//
// Deduplica por huella para no repetir la misma señal veinte veces, y recorta
// cada línea para que el diagnóstico quepa en una pantalla.
func detectRuntimeInstall(text string) []string {
	var hits []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		for _, s := range installSigs {
			if strings.Contains(low, s.needle) && !seen[s.needle] {
				seen[s.needle] = true
				hits = append(hits, fmt.Sprintf("%s  →  %s", clip(line, 96), s.why))
				break
			}
		}
	}
	return hits
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// installFindingsMsg arma el mensaje que se muestra cuando el guardián dispara.
func installFindingsMsg(service string, hits []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "server %q is trying to INSTALL dependencies at runtime, inside the microVM:\n", service)
	for _, h := range hits {
		fmt.Fprintf(&b, "    · %s\n", h)
	}
	b.WriteString("\nThat installs into the ephemeral overlay (dies with the session and reinstalls every time)\n")
	b.WriteString("instead of into the shared image, and with no internet egress the attempt fails and\n")
	b.WriteString("exhausts the guest's memory. Bake it into the image when building it:\n")
	b.WriteString("    scripts/80-mcp-image.sh …  -P \"<pip packages>\"   (e.g. -P semgrep)\n")
	b.WriteString("                               -n \"<npm packages>\"\n")
	b.WriteString("Rebuild the image and reimport. To import anyway: --allow-runtime-install")
	return b.String()
}

// looksLikePath adivina si un parámetro pide una ruta del sistema de ficheros,
// para rellenarlo con algo real y que un escáner llegue a ejecutarse.
func looksLikePath(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"path", "dir", "directory", "file", "folder", "root", "cwd", "target", "location"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func sampleValue(name, typ string) any {
	switch typ {
	case "number", "integer":
		return 1
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default: // string o desconocido
		if looksLikePath(name) {
			// Un directorio que SÍ tiene código en cualquier imagen de servicio:
			// /tmp está vacío y un escáner lo despacha sin ejecutar su binario —y
			// sin disparar la instalación perezosa que buscamos—. /usr/local/lib
			// lleva los node_modules y el código que se horneó al construir.
			return "/usr/local/lib"
		}
		return "x"
	}
}

// minimalArgs construye los argumentos mínimos para EJERCER una herramienta a
// partir de su esquema: rellena los campos requeridos con un valor de su tipo.
// No busca que la herramienta tenga éxito, solo que llegue a ejecutar su trabajo
// —y con él, cualquier instalación perezosa que esconda—.
func minimalArgs(schema json.RawMessage) map[string]any {
	args := map[string]any{}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &s) != nil {
		return args
	}
	want := s.Required
	if len(want) == 0 {
		for k := range s.Properties {
			want = append(want, k)
		}
	}
	for _, name := range want {
		var p struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(s.Properties[name], &p)
		args[name] = sampleValue(name, p.Type)
	}
	return args
}

// exerciseTools llama a cada herramienta con argumentos mínimos y devuelve las
// respuestas concatenadas, para escanearlas junto con la consola. Es lo que
// dispara las instalaciones que solo ocurren al USAR una herramienta.
func exerciseTools(post poster, tools []api.ToolSpec) string {
	sid, _, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kling-verify","version":"1"}}}`)
	if err != nil {
		return ""
	}
	_, _, _ = post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var out strings.Builder
	for i, t := range tools {
		args, _ := json.Marshal(minimalArgs(t.InputSchema))
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
			100+i, t.Name, args)
		if _, raw, err := post(sid, body); err == nil {
			out.Write(api.MCPPayload(raw))
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// mcpVerify PRUEBA una imagen sin importarla: la arranca como una microVM
// desechable (aislada, igual que en producción), ejerce sus herramientas y vigila
// que no intente instalar nada en caliente. No congela ningún snapshot.
func mcpVerify(args []string) error {
	fs := flag.NewFlagSet("mcp verify", flag.ExitOnError)
	host := hostFlag(fs)
	image := fs.String("image", "", "image to test (default: the given name)")
	mem := fs.Int("mem", 0, "test memory in MiB")
	cpus := fs.Int("cpus", 0, "test vCPUs")
	egress := fs.String("egress", "", "network egress: none | internet (default none, like production)")
	var volumes volumeFlag
	fs.Var(&volumes, "volume", "volume to mount: name[:/mountpoint][:ro] (repeatable)")
	mount := fs.String("mount", "", "where to mount the volume (default /data; only with one)")
	volRO := fs.Bool("volume-ro", false, "mount it read-only")
	wait := fs.Duration("wait", 45*time.Second, "maximum wait for the server to start")
	settle := fs.Duration("settle", 90*time.Second, "how long to watch the console after exercising the tools (installs take a while to fail)")
	keep := fs.Bool("keep", false, "don't destroy the test microVM when done")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling mcp verify <service> [-image image]")
	}
	importVols, err := volumeSet(volumes, *mount, *volRO)
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	img := config.Or(*image, name)

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	c := api.NewClient(cfg.Host(*host))
	tmpl := name + "-verify"

	fmt.Printf("Testing %q from image %q (without importing)\n\n", name, img)

	fmt.Printf("  1/4  starting in isolation... ")
	mc, err := c.Run(ctx, api.RunRequest{
		Name:    tmpl,
		Image:   img,
		MemMiB:  config.Or(*mem, cfg.Defaults.MemMiB, 256),
		VCPUs:   config.Or(*cpus, cfg.Defaults.VCPUs, 1),
		Egress:  config.Or(*egress, cfg.Defaults.Egress, "none"),
		Volumes: importVols,
		Labels:  map[string]string{api.LabelService: name},
	})
	if err != nil {
		fmt.Println("✗")
		return fmt.Errorf("couldn't start the test: %w", err)
	}
	fmt.Printf("✓ %s\n", mc.ID[:8])
	remove := func() {
		if !*keep {
			_ = c.Remove(context.WithoutCancel(ctx), mc.ID)
		}
	}

	fmt.Printf("  2/4  waiting for the server... ")
	if err := waitGuest(ctx, c, mc.ID, *wait); err != nil {
		fmt.Println("✗")
		fmt.Printf("\nThe server didn't open port 8080. Log:\n  kling logs %s\n", tmpl)
		remove()
		return err
	}
	post := guestPost(ctx, c, mc.ID)
	_, tools, ierr := introspectWith(post)
	if ierr != nil {
		fmt.Println("✗")
		remove()
		return fmt.Errorf("introspection: %w", ierr)
	}
	fmt.Printf("✓ %d tool(s)\n", len(tools))

	fmt.Printf("  3/4  exercising the tools... ")
	// Acotado: una herramienta que se cuelgue —un escaneo enorme— no debe colgar
	// el verify. El disparo de instalación ocurre al principio del trabajo, así
	// que unos minutos sobran para provocarlo.
	exCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	resp := exerciseTools(guestPost(exCtx, c, mc.ID), tools)
	cancel()
	fmt.Println("✓")

	fmt.Printf("  4/4  watching the console (up to %s)... ", *settle)
	deadline := time.Now().Add(*settle)
	var hits []string
	for {
		logtxt, _ := c.Logs(ctx, mc.ID, 0)
		hits = detectRuntimeInstall(logtxt + "\n" + resp)
		if len(hits) > 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			remove()
			return ctx.Err()
		}
	}
	remove()
	if len(hits) > 0 {
		fmt.Println("✗")
		fmt.Println()
		return fmt.Errorf("%s", installFindingsMsg(name, hits))
	}
	fmt.Println("✓")
	fmt.Printf("\n%s didn't try to install anything at runtime: it takes everything from the shared image.\n", name)
	fmt.Printf("(exercised %d tool(s) and watched the console for %s)\n", len(tools), *settle)
	return nil
}
