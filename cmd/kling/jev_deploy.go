package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/jev"
)

// `kling jev deploy|ls|rm`: JEV como tarea serverless de kindling
// (docs/jev-serverless.md), la alternativa a servirlo en proceso dentro de
// `kling ai serve`. Cada tarea es su PROPIA imagen (el .jev va horneado
// dentro, como el GGUF de un modelo VON) y su propio dorado congelado: el
// planificador la despierta con la primera petición y la congela al quedarse
// ociosa, exactamente como a un VON. No hace falta ni entiende de llama.cpp:
// el invitado es kling-jev (cmd/kling-jev), un binario estático que solo sabe
// de JEV.

// LabelTask marca el dorado y sus réplicas con el nombre de la tarea, para
// `kling jev ls` y para que un humano que mire `kling ps` sepa qué es.
const jevLabelTask = "jev.task"

func jevLabels(name string, extra map[string]string) map[string]string {
	return api.MergeLabels(extra, map[string]string{
		jevLabelTask:     name,
		api.LabelPorts:   strconv.Itoa(jev.GuestPort),
		api.LabelService: name,
	})
}

func cmdJevDeploy(args []string) error {
	fs := flag.NewFlagSet("jev deploy", flag.ExitOnError)
	host := hostFlag(fs)
	modelPath := fs.String("model", "", "the .jev to deploy (required)")
	slotsPath := fs.String("slots", "", "optional .jevs (slots, docs/domotica.md)")
	mem := fs.Int("mem", 64, "microVM memory in MiB (32-64 is usually enough; see docs/jev-serverless.md)")
	vcpus := fs.Int("vcpus", 1, "microVM vCPUs")
	warmText := fs.String("warm-text", "hello world", "text sent once before freezing, to touch the model's pages")
	replace := fs.Bool("replace", false, "replace the golden snapshot if it exists")
	rebuild := fs.Bool("rebuild", false, "rebuild the image even if one with this name exists")
	allowExec := fs.Bool("allow-exec", false, "keep kling exec/cp working in this task's machines (debugging)")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for kling-jev to answer /healthz")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *modelPath == "" {
		return fmt.Errorf("usage: kling jev deploy <task> -model m.jev [-slots s.jevs] [-mem 64] [-vcpus 1]")
	}
	name := fs.Arg(0)
	modelBytes, err := os.ReadFile(*modelPath)
	if err != nil {
		return err
	}
	if _, err := jev.Load(bytes.NewReader(modelBytes)); err != nil {
		return fmt.Errorf("%s does not load as a .jev: %w", *modelPath, err)
	}
	spec := JEVSpec{ModelB64: base64.StdEncoding.EncodeToString(modelBytes)}
	if *slotsPath != "" {
		sb, err := os.ReadFile(*slotsPath)
		if err != nil {
			return err
		}
		spec.SlotsB64 = base64.StdEncoding.EncodeToString(sb)
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))

	if !*replace {
		if snaps, err := c.Snapshots(ctx); err == nil {
			for _, s := range snaps {
				if s.Name == name {
					return fmt.Errorf("snapshot %q already exists (use -replace, or kling jev rm %s)", name, name)
				}
			}
		}
	}

	if !*rebuild {
		if imgs, err := c.Images(ctx); err == nil {
			for _, img := range imgs {
				if img.Name == name {
					return fmt.Errorf("image %q already exists (use -rebuild to build it again with this model)", name)
				}
			}
		}
	}

	fmt.Printf("Building image %q (kling-jev + %s)...\n", name, *modelPath)
	specJSON, _ := json.Marshal(spec)
	if _, err := c.BuildImage(ctx, api.BuildImageRequest{Name: name, Base: "min", Builder: "jev", Spec: specJSON}); err != nil {
		return err
	}

	fmt.Printf("Making the golden snapshot (%d vCPU, %d MiB)...\n", *vcpus, *mem)
	t0 := time.Now()
	g, err := makeJEVGolden(ctx, c, jevGoldenOptions{
		Image: name, Snapshot: name, VCPUs: *vcpus, MemMiB: *mem,
		AllowExec: *allowExec, Replace: *replace, Wait: *wait, WarmText: *warmText,
		Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Printf("✓ %s  golden snapshot of task %s  (%s of memory, %s in total)\n",
		g.Snapshot.Name, name, human(g.Snapshot.MemBytes), time.Since(t0).Round(time.Second))
	fmt.Println()
	fmt.Println("Add it to the ai gateway's registry (docs/ai-gateway.md) as a microvm-backed model:")
	fmt.Printf("  {\"models\": {%q: {\"kind\": \"jev\", \"backend\": \"microvm\", \"snapshot\": %q}}}\n", name, name)
	return nil
}

func cmdJevLs(args []string) error {
	fs := flag.NewFlagSet("jev ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
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
	var tasks []*api.Snapshot
	for _, s := range snaps {
		if s.Labels[jevLabelTask] != "" {
			tasks = append(tasks, s)
		}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(tasks)
	}
	if len(tasks) == 0 {
		fmt.Println("no jev tasks deployed (kling jev deploy <task> -model m.jev)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "TASK\tSNAPSHOT\tMEMORY\tREPLICAS RUNNING")
	for _, s := range tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", s.Labels[jevLabelTask], s.Name, human(s.MemBytes), s.Instances)
	}
	return tw.Flush()
}

func cmdJevRm(args []string) error {
	fs := flag.NewFlagSet("jev rm", flag.ExitOnError)
	host := hostFlag(fs)
	keepImage := fs.Bool("keep-image", false, "remove only the golden snapshot")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling jev rm <task> [-keep-image]")
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

// jevGoldenOptions es von.GoldenOptions recortado a lo que un dorado de JEV
// necesita: sin prefijos ni Kind de VON, con el puerto y la ruta de salud de
// kling-jev en vez de las de llama-server.
type jevGoldenOptions struct {
	Image, Snapshot    string
	VCPUs, MemMiB      int
	AllowExec, Replace bool
	Wait               time.Duration
	WarmText           string
	Log                func(format string, args ...any)
}

type jevGoldenResult struct {
	Snapshot *api.Snapshot
	BootMS   int64
	LoadMS   int64
}

// makeJEVGolden arranca una microVM de la imagen, espera a que kling-jev
// conteste /healthz, la calienta con una clasificación de verdad (toca las
// páginas del modelo y del binario antes de congelar) y la congela como
// dorado. Es el mismo patrón que von.MakeGolden, pero sin nada de llama.cpp:
// JEV no necesita gramática, caché de prompts ni prefijos, así que
// reescribirlo aquí, más corto, es más claro que forzarlo dentro de
// von.GoldenOptions.
func makeJEVGolden(ctx context.Context, c *api.Client, o jevGoldenOptions) (*jevGoldenResult, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Wait == 0 {
		o.Wait = 2 * time.Minute
	}
	name := fmt.Sprintf("%s-golden-%s", o.Snapshot, strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36))
	mc, err := c.Run(ctx, api.RunRequest{
		Name: name, Image: o.Image, VCPUs: o.VCPUs, MemMiB: o.MemMiB,
		Egress: "none", Labels: jevLabels(o.Snapshot, nil), AllowExec: o.AllowExec,
	})
	if err != nil {
		return nil, fmt.Errorf("booting %s: %w", o.Image, err)
	}
	res := &jevGoldenResult{BootMS: mc.BootMS}
	defer func() {
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = c.Remove(rc, mc.ID)
	}()
	logf("booted %s in %d ms; waiting for kling-jev...", mc.Name, mc.BootMS)

	t0 := time.Now()
	if err := waitJEVHealthy(ctx, c, mc.ID, o.Wait); err != nil {
		return nil, fmt.Errorf("%w\nsee the service log with:  kling exec %s -- tail -50 /var/log/service.log", err, mc.Name)
	}
	res.LoadMS = time.Since(t0).Milliseconds()
	logf("kling-jev ready in %s; warming up...", time.Since(t0).Round(time.Millisecond))

	body, _ := json.Marshal(map[string]any{"text": o.WarmText})
	resp, err := c.Guest(ctx, mc.ID, api.GuestRequest{
		Port: jev.GuestPort, Path: "/v1/classify", Method: http.MethodPost, Body: string(body),
	})
	if err != nil {
		return nil, fmt.Errorf("warm-up: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("warm-up: kling-jev answered %d: %s", resp.Status, resp.Body)
	}
	logf("warm-up answered %s", truncate(resp.Body, 200))

	if sq, err := c.Squeeze(ctx, mc.ID); err == nil {
		logf("returned %d MiB of free memory to the host before freezing", sq.ReclaimedMiB)
	}
	snap, err := c.Commit(ctx, mc.ID, o.Snapshot, o.Replace)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	res.Snapshot = snap
	return res, nil
}

// waitJEVHealthy espera a que el puerto de kling-jev abra y luego a que
// /healthz conteste 200: igual que von.WaitReady, pero contra el agente de
// JEV en vez de llama-server.
func waitJEVHealthy(ctx context.Context, c *api.Client, ref string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if _, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: jev.GuestPort, WaitMS: int(timeout / time.Millisecond), ProbeOnly: true,
	}); err != nil {
		return fmt.Errorf("kling-jev did not open port %d: %w", jev.GuestPort, err)
	}
	var last error
	for time.Now().Before(deadline) {
		resp, err := c.Guest(ctx, ref, api.GuestRequest{Port: jev.GuestPort, Path: "/healthz", Method: http.MethodGet})
		switch {
		case err != nil:
			last = err
		case resp.Status != http.StatusOK:
			last = fmt.Errorf("/healthz answered %d", resp.Status)
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("kling-jev not ready after %s: %w", timeout, last)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
