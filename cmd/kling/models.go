package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/von"
)

// MODELOS VON: `kling models`.
//
// Un modelo es una imagen construida con el constructor "llm" (llama-server y
// un GGUF fijado) y un snapshot dorado del mismo nombre, congelado con el
// modelo ya cargado y caliente. Se sirve con `kling run -from <nombre>`: la
// réplica contesta en cuanto termina el thaw. Todo el porqué en docs/von.md.

func cmdModels(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling models <ls|add|ask|embed|rm> [...]")
	}
	switch args[0] {
	case "ls", "list":
		return modelsList(args[1:])
	case "add":
		return modelsAdd(args[1:])
	case "ask":
		return modelsAsk(args[1:])
	case "embed":
		return modelsEmbed(args[1:])
	case "rm", "remove":
		return modelsRm(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q: use ls, add, ask, embed or rm", args[0])
	}
}

// modelsList enseña el catálogo y los modelos ya servibles en el daemon (los
// snapshots con la etiqueta von.model).
func modelsList(args []string) error {
	fs := flag.NewFlagSet("models ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()

	snaps, err := api.NewClient(hostOf(*host)).Snapshots(ctx)
	if err != nil {
		return err
	}
	var mine []*api.Snapshot
	for _, s := range snaps {
		if s.Labels[von.LabelModel] != "" {
			mine = append(mine, s)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })

	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"catalog": von.Catalog, "installed": mine, "llama_cpp": von.LlamaTag, "port": von.Port,
		})
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CATALOG\tQUANT\tKIND\tGGUF\tDEFAULT CPU/MEM\tLICENSE")
	for _, m := range von.Catalog {
		lic := m.License
		if !m.Open() {
			lic += " (not free to use and redistribute: needs -accept-license " + m.License + ")"
		}
		kind := "chat"
		if m.Kind != "" {
			kind = m.Kind
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%dMiB\t%s\n", m.ID, m.Quant, kind, human(m.Size), m.VCPUs, m.MemMiB, lic)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println()
	if len(mine) == 0 {
		fmt.Println("No models on this daemon yet. Add one:  kling models add von-smol -model smollm2-360m-instruct")
		return nil
	}
	fmt.Fprintln(tw, "NAME\tMODEL\tCPU/MEM\tSNAPSHOT\tPREFIXES\tINSTANCES")
	for _, s := range mine {
		ref := s.Labels[von.LabelModel]
		if k := s.Labels[von.LabelKind]; k != "" {
			ref += " (" + k + ")"
		}
		pf := s.Labels[von.LabelPrefixes]
		if pf == "" {
			pf = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d/%dMiB\t%s\t%s\t%d\n", s.Name, ref, s.VCPUs, s.MemMiB, human(s.MemBytes), pf, s.Instances)
	}
	return tw.Flush()
}

// modelsAdd construye la imagen (si hace falta) y el dorado de un modelo.
func modelsAdd(args []string) error {
	fs := flag.NewFlagSet("models add", flag.ExitOnError)
	host := hostFlag(fs)
	model := fs.String("model", "", "catalog model ("+strings.Join(von.IDs(), ", ")+")")
	quant := fs.String("quant", "", "quantization (default q8_0)")
	url := fs.String("url", "", "a GGUF of your own: https://huggingface.co/<org>/<repo>/resolve/<commit>/<file>.gguf")
	sum := fs.String("sha256", "", "sha256 of the -url file (required with -url)")
	ctxSize := fs.Int("ctx", 0, fmt.Sprintf("context size in tokens (default %d); the KV cache is reserved for all of it", von.DefaultCtx))
	parallel := fs.Int("parallel", 0, "requests a replica serves at once (default 1; prefer more replicas)")
	threads := fs.Int("threads", 0, "compute threads (default: one per vCPU)")
	cpus := fs.Int("cpus", 0, "vCPUs of the microVM (default: the model's)")
	mem := fs.Int("mem", 0, "memory in MiB (default: the model's)")
	cpuPct := fs.Int("cpu-pct", 0, "CPU ceiling as a percentage of one core, kept by the snapshot (default: 100 per vCPU)")
	rebuild := fs.Bool("rebuild", false, "rebuild the image even if it exists")
	replace := fs.Bool("replace", false, "replace the golden snapshot if it exists")
	allowExec := fs.Bool("allow-exec", true, "keep kling exec/cp working in the model's machines (debugging)")
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for the model to load")
	acceptLicense := fs.String("accept-license", "", "build a catalog model outside the default catalog, accepting its license (give its id)")
	buildOnly := fs.Bool("build-only", false, "build the image and stop: e.g. to copy it to a macOS daemon, which cannot build")
	cacheRAM := fs.Int("cache-ram", -1, fmt.Sprintf("MiB of llama-server's prompt cache, added to the memory (default %d; 0 = off)", von.DefaultCacheRAM))
	var prefixes stringsFlag
	fs.Var(&prefixes, "prefix", "file with a task's system prompt to leave evaluated in the golden snapshot (repeatable)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling models add <name> -model <id> [-quant q8_0] [-ctx N] [-cpus N] [-mem MiB] [-prefix system.txt]...")
	}
	name := fs.Arg(0)
	spec := von.Spec{Model: *model, Quant: *quant, URL: *url, SHA256: *sum,
		Ctx: *ctxSize, Parallel: *parallel, Threads: *threads, AcceptLicense: *acceptLicense}
	if *cacheRAM >= 0 {
		spec.CacheRAM = cacheRAM
	}
	res, err := spec.Resolve()
	if err != nil {
		return err
	}
	if res.Kind == von.KindEmbed && len(prefixes) > 0 {
		return fmt.Errorf("-prefix only applies to instruct models: %s is an encoder (kind embed) and has no system prompt to cache", name)
	}
	// La receta guarda la caché de prompts siempre, también la de por defecto:
	// así una imagen dice con qué --cache-ram arranca aunque el defecto cambie.
	spec.CacheRAM = &res.CacheRAM
	var pre []von.Prefix
	for _, f := range prefixes {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if len(b) == 0 || len(b) > 64<<10 {
			return fmt.Errorf("-prefix %s: must hold a system prompt of 1 byte to 64 KiB", f)
		}
		pre = append(pre, von.Prefix{System: string(b)})
	}
	if len(pre) > 1 && res.CacheRAM == 0 {
		fmt.Println("Note: without a prompt cache (-cache-ram 0) only the last -prefix stays evaluated in the golden.")
	}
	if res.Model != nil && !res.Model.Open() {
		fmt.Printf("License of %s: %s (%s), accepted with -accept-license. It does not allow free use and redistribution: check it before serving or copying this image.\n",
			res.Ref, res.Model.License, res.Model.LicenseURL)
	}
	vcpus, memMiB := *cpus, *mem
	if res.Model != nil {
		if vcpus == 0 {
			vcpus = res.Model.VCPUs
		}
		if memMiB == 0 {
			// La caché de prompts va aparte de lo medido para el modelo: sin
			// sumarla, un dorado de 1,5B en 1536 MiB se quedaba sin memoria al
			// llenarla (docs/von-cpu.md).
			memMiB = res.Model.MemMiB + res.CacheRAM
		}
	}
	if vcpus == 0 {
		vcpus = 2
	}
	if memMiB == 0 {
		// Un GGUF propio: no se sabe su tamaño hasta bajarlo. 1 GiB cubre hasta
		// ~0.5B parámetros en Q8_0; para algo mayor, -mem.
		memMiB = 1024 + res.CacheRAM
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	// El dorado antes que nada: descubrir al final, tras construir, que el
	// nombre estaba cogido es tirar minutos.
	if !*replace && !*buildOnly {
		if snaps, err := c.Snapshots(ctx); err == nil {
			for _, s := range snaps {
				if s.Name == name {
					return fmt.Errorf("snapshot %q already exists (use -replace, or kling models rm %s)", name, name)
				}
			}
		}
	}

	if err := ensureModelImage(ctx, c, name, spec, res, *rebuild); err != nil {
		return err
	}
	if *buildOnly {
		if len(pre) > 0 {
			fmt.Printf("Warning: -prefix is not applied with -build-only (no golden snapshot is made here); pass -prefix again in the kling models add below.\n")
		}
		fmt.Println("Copy it to another daemon and make the golden snapshot there:")
		fmt.Printf("  kling images copy %s -from <this daemon> -to <that daemon>\n", name)
		fmt.Printf("  kling models add -H <that daemon> %s %s\n", name, modelFlags(spec))
		return nil
	}

	fmt.Printf("Making the golden snapshot (%d vCPU, %d MiB)...\n", vcpus, memMiB)
	t0 := time.Now()
	var labels map[string]string
	if res.Kind != "" {
		labels = map[string]string{von.LabelKind: res.Kind}
	}
	g, err := von.MakeGolden(ctx, c, von.GoldenOptions{
		Image: name, Snapshot: name, Ref: res.Ref, VCPUs: vcpus, MemMiB: memMiB, CPUPct: *cpuPct,
		Kind: res.Kind, Labels: labels,
		AllowExec: *allowExec, Replace: *replace, Wait: *wait, Prefixes: pre,
		Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Printf("✓ %s  golden snapshot of %s  (%s of memory, %s in total)\n",
		g.Snapshot.Name, res.Ref, human(g.Snapshot.MemBytes), time.Since(t0).Round(time.Second))
	if len(pre) > 0 {
		fmt.Printf("  %d task prefix(es) already evaluated (%v tokens): the first request of those tasks only evaluates its own text.\n",
			len(pre), g.PrefixTokens)
	}
	fmt.Println()
	fmt.Println("Serve it:")
	fmt.Printf("  kling run -from %s -name %s-1\n", name, name)
	if res.Kind == von.KindEmbed {
		fmt.Printf("  kling models embed %s-1 \"turn on the lights\"\n", name)
		fmt.Printf("Embeddings API (POST /v1/embeddings) on port %d of each replica.\n", von.Port)
		return nil
	}
	fmt.Printf("  kling models ask %s-1 \"What is a microVM?\"\n", name)
	fmt.Printf("OpenAI-compatible API on port %d of each replica (kling inspect <ref> for the address).\n", von.Port)
	return nil
}

// modelFlags repite los flags del modelo, para las instrucciones que se imprimen.
func modelFlags(s von.Spec) string {
	var out []string
	if s.Model != "" {
		out = append(out, "-model "+s.Model)
	}
	if s.Quant != "" {
		out = append(out, "-quant "+s.Quant)
	}
	if s.URL != "" {
		out = append(out, "-url "+s.URL, "-sha256 "+s.SHA256)
	}
	if s.AcceptLicense != "" {
		out = append(out, "-accept-license "+s.AcceptLicense)
	}
	if s.Ctx != 0 {
		out = append(out, fmt.Sprintf("-ctx %d", s.Ctx))
	}
	if s.Parallel != 0 {
		out = append(out, fmt.Sprintf("-parallel %d", s.Parallel))
	}
	if s.Threads != 0 {
		out = append(out, fmt.Sprintf("-threads %d", s.Threads))
	}
	if s.CacheRAM != nil && *s.CacheRAM != von.DefaultCacheRAM {
		out = append(out, fmt.Sprintf("-cache-ram %d", *s.CacheRAM))
	}
	return strings.Join(out, " ")
}

// ensureModelImage construye la imagen con el constructor llm, o reutiliza la
// que hay si es del mismo modelo. Reutilizar es lo que permite hacer el dorado
// en un daemon sin constructores: en macOS la imagen llega con `kling images
// copy` desde un host Linux, y el dorado se hace allí (es propio de cada
// backend y de cada host).
func ensureModelImage(ctx context.Context, c *api.Client, name string, spec von.Spec, res von.Resolved, rebuild bool) error {
	if !rebuild {
		imgs, err := c.Images(ctx)
		if err != nil {
			return err
		}
		for _, img := range imgs {
			if img.Name != name {
				continue
			}
			if rec, err := c.ImageRecipe(ctx, name); err == nil && rec.Builder != "" {
				var prev von.Spec
				_ = json.Unmarshal(rec.Spec, &prev)
				// La aceptación de la licencia es de quien construye ahora, no
				// de la receta: una imagen hecha antes de existir el flag vale.
				prev.AcceptLicense = spec.AcceptLicense
				if prev.CacheRAM == nil {
					// Receta de antes de v0.12: su run.sh lleva --cache-ram 0.
					cero := 0
					prev.CacheRAM = &cero
				}
				pr, perr := prev.Resolve()
				if perr == nil && rec.Builder == "llm" && pr.Ref == res.Ref && pr.CacheRAM != res.CacheRAM {
					return fmt.Errorf("image %q was built with a %d MiB prompt cache, not %d: pass -cache-ram %d to reuse it, or -rebuild",
						name, pr.CacheRAM, res.CacheRAM, pr.CacheRAM)
				}
				if rec.Builder != "llm" || perr != nil || pr.Ref != res.Ref || pr.Ctx != res.Ctx || pr.Kind != res.Kind ||
					pr.Parallel != res.Parallel || pr.Threads != res.Threads {
					return fmt.Errorf("image %q exists but was built for something else (builder %q, model %q): use another name or -rebuild",
						name, rec.Builder, pr.Ref)
				}
			}
			fmt.Printf("Image %q already exists; reusing it (-rebuild to build it again).\n", name)
			return nil
		}
	}
	fmt.Printf("Building image %q: llama.cpp %s + %s on base %s.\n", name, von.LlamaTag, res.Ref, von.BaseImage)
	fmt.Print("  (downloads llama.cpp and the model the first time; the first model also builds the base)... ")
	b, _ := json.Marshal(spec)
	t0 := time.Now()
	out, err := c.BuildImage(ctx, api.BuildImageRequest{
		Name: name, Base: von.BaseImage, Builder: "llm", Spec: b,
	})
	if err != nil {
		fmt.Println("✗")
		var se *api.StatusError
		if errors.As(err, &se) && (se.Code == 412 || se.Code == 501) {
			// 412: un daemon Linux sin el constructor instalado. 501: macOS, que
			// no construye imágenes. En los dos casos, el camino es el mismo.
			return fmt.Errorf("%w\nbuild the image on a Linux daemon (the llm builder ships with it: make deploy) and copy it here:\n"+
				"  kling models add -H ssh://<linux host> %s %s -build-only\n  kling images copy %s -from ssh://<linux host>",
				err, name, modelFlags(spec), name)
		}
		return err
	}
	fmt.Printf("✓ %s\n", time.Since(t0).Round(time.Second))
	for _, l := range strings.Split(strings.TrimSpace(out.Output), "\n") {
		if strings.HasPrefix(l, "downloaded ") || strings.HasPrefix(l, "imagen ") || strings.HasPrefix(l, "model ") {
			fmt.Println("  " + l)
		}
	}
	return nil
}

// modelsAsk manda un prompt a una réplica y enseña la respuesta y los tiempos
// que da llama-server. Va por el proxy del daemon, así que funciona igual en
// local y por SSH.
func modelsAsk(args []string) error {
	fs := flag.NewFlagSet("models ask", flag.ExitOnError)
	host := hostFlag(fs)
	maxTokens := fs.Int("max-tokens", 256, "maximum tokens to generate")
	temp := fs.Float64("temperature", -1, "sampling temperature (default: the server's)")
	seed := fs.Int64("seed", -1, "sampling seed (default: a fresh one per request)")
	system := fs.String("system", "", "system prompt")
	wait := fs.Duration("wait", 0, "wait this long for the model to load first (a cold-booted machine)")
	asJSON := fs.Bool("json", false, "print the full response and the wall time as JSON")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling models ask <machine> [-max-tokens N] <prompt...>")
	}
	ref, prompt := fs.Arg(0), strings.Join(fs.Args()[1:], " ")

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	t0 := time.Now()
	if *wait > 0 {
		if err := von.WaitReady(ctx, c, ref, *wait); err != nil {
			return err
		}
	}
	req := von.ChatRequest{MaxTokens: *maxTokens}
	if *system != "" {
		req.Messages = append(req.Messages, von.Message{Role: "system", Content: *system})
	}
	req.Messages = append(req.Messages, von.Message{Role: "user", Content: prompt})
	if *temp >= 0 {
		req.Temperature = temp
	}
	if *seed >= 0 {
		req.Seed = seed
	}
	resp, dur, err := von.Chat(ctx, c, ref, req)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"response": resp, "request_ms": dur.Milliseconds(), "total_ms": time.Since(t0).Milliseconds(),
		})
	}
	fmt.Println(strings.TrimSpace(resp.Text()))
	if t := resp.Timings; t != nil {
		fmt.Fprintf(os.Stderr, "\n[%s: prompt %d tok at %.0f tok/s, generated %d tok at %.1f tok/s, %d ms end to end]\n",
			resp.Model, t.PromptN, t.PromptPerSecond, t.PredictedN, t.PredictedPerSecond, dur.Milliseconds())
	}
	return nil
}

// modelsEmbed pide el vector de un texto a un codificador (kind embed) y
// enseña su tamaño, su norma y lo que tardó: para comprobar una réplica, no
// para usar los vectores (eso es pkg/codificador).
func modelsEmbed(args []string) error {
	fs := flag.NewFlagSet("models embed", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "print the full response and the wall time as JSON")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling models embed <machine> <text...>")
	}
	ref, text := fs.Arg(0), strings.Join(fs.Args()[1:], " ")
	ctx, stop := ctxWithSignals()
	defer stop()
	resp, dur, err := von.Embed(ctx, api.NewClient(hostOf(*host)), ref, []string{text})
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"response": resp, "request_ms": dur.Milliseconds()})
	}
	v := resp.Data[0].Embedding
	norm := 0.0
	for _, x := range v {
		norm += x * x
	}
	head := v[:min(4, len(v))]
	fmt.Printf("%s: %d dimensions, norm %.4f, %d tokens, %.1f ms end to end\nfirst values: %v\n",
		resp.Model, len(v), math.Sqrt(norm), resp.Usage.PromptTokens, float64(dur.Microseconds())/1000, head)
	return nil
}

// modelsRm quita el dorado y la imagen de un modelo. Las réplicas vivas lo
// impiden (el daemon se niega a borrar un snapshot con instancias).
func modelsRm(args []string) error {
	fs := flag.NewFlagSet("models rm", flag.ExitOnError)
	host := hostFlag(fs)
	keepImage := fs.Bool("keep-image", false, "remove only the golden snapshot")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling models rm <name> [-keep-image]")
	}
	name := fs.Arg(0)
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	var errs []error
	if err := c.RemoveSnapshot(ctx, name); err != nil {
		errs = append(errs, fmt.Errorf("snapshot: %w", err))
	} else {
		fmt.Printf("removed snapshot %s\n", name)
	}
	if !*keepImage {
		if err := c.RemoveImage(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("image: %w", err))
		} else {
			fmt.Printf("removed image %s\n", name)
		}
	}
	return errors.Join(errs...)
}
