package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// `kling chispa deploy|ls|rm`: Chispa como tarea serverless de kindling
// (docs/chispa-serverless.md), la alternativa a servirlo en proceso dentro de
// `kling ai serve`. Cada tarea es su PROPIA imagen (el .chispa va horneado
// dentro, como el GGUF de un modelo VON) y su propio dorado congelado: el
// planificador la despierta con la primera petición y la congela al quedarse
// ociosa, exactamente como a un VON. No hace falta ni entiende de llama.cpp:
// el invitado es kling-chispa (cmd/kling-chispa), un binario estático que solo sabe
// de Chispa.

// LabelTask marca el dorado y sus réplicas con el nombre de la tarea, para
// `kling chispa ls` y para que un humano que mire `kling ps` sepa qué es.
const chispaLabelTask = "chispa.task"

func chispaLabels(name string, extra map[string]string) map[string]string {
	return api.MergeLabels(extra, map[string]string{
		chispaLabelTask:  name,
		api.LabelPorts:   strconv.Itoa(chispa.GuestPort),
		api.LabelService: name,
	})
}

func cmdChispaDeploy(args []string) error {
	fs := flag.NewFlagSet("chispa deploy", flag.ExitOnError)
	host := hostFlag(fs)
	modelPath := fs.String("model", "", "the .chispa to deploy (required)")
	slotsPath := fs.String("slots", "", "optional .chispas (slots, docs/domotica.md)")
	// 128 y no 64: un modelo de 28 etiquetas tardaba 1,96 s en calentar y, tras
	// el thaw, rechazaba conexiones con 64 MiB (se quedaba corto de memoria).
	mem := fs.Int("mem", 128, "microVM memory in MiB (64-128 is usually enough; see docs/chispa-serverless.md)")
	vcpus := fs.Int("vcpus", 1, "microVM vCPUs")
	warmText := fs.String("warm-text", "hello world", "text sent once before freezing, to touch the model's pages")
	replace := fs.Bool("replace", false, "replace the golden snapshot if it exists")
	rebuild := fs.Bool("rebuild", false, "rebuild the image even if one with this name exists")
	reuse := fs.Bool("reuse-image", false, "the image already exists with this same -model (built on a Linux host and brought with `kling images copy`): make only the golden snapshot")
	allowExec := fs.Bool("allow-exec", false, "keep kling exec/cp working in this task's machines (debugging)")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for kling-chispa to answer /healthz")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *modelPath == "" {
		return fmt.Errorf("usage: kling chispa deploy <task> -model m.chispa [-slots s.chispas] [-mem 128] [-vcpus 1]")
	}
	name := fs.Arg(0)
	modelBytes, err := os.ReadFile(*modelPath)
	if err != nil {
		return err
	}
	var slotsBytes []byte
	if *slotsPath != "" {
		if slotsBytes, err = os.ReadFile(*slotsPath); err != nil {
			return err
		}
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	if err := chispaDeploy(ctx, c, chispaDeployOptions{
		Name: name, Model: modelBytes, ModelName: *modelPath, Slots: slotsBytes, MemMiB: *mem, VCPUs: *vcpus,
		WarmText: *warmText, Replace: *replace, Rebuild: *rebuild, Reuse: *reuse, AllowExec: *allowExec, Wait: *wait,
	}); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("Add it to the ai gateway's registry (docs/ai-gateway.md) as a microvm-backed model:")
	fmt.Printf("  {\"models\": {%q: {\"kind\": \"chispa\", \"backend\": \"microvm\", \"snapshot\": %q}}}\n", name, name)
	return nil
}

// chispaDeployOptions es lo que necesita un despliegue: lo usan `kling chispa
// deploy` y `kling ai retrain` (una versión nueva de una tarea microvm es un
// dorado nuevo, <snapshot>-vN).
type chispaDeployOptions struct {
	Name             string
	Model            []byte
	ModelName        string // para los mensajes
	Slots            []byte
	MemMiB, VCPUs    int
	WarmText         string
	Replace, Rebuild bool
	Reuse            bool // la imagen ya existe con este mismo Model; solo el dorado
	AllowExec        bool
	Wait             time.Duration
}

// chispaDeploy construye la imagen (kling-chispa + el .chispa), hace el dorado
// y graba en él el registro de despliegue (etiquetas y sha256).
func chispaDeploy(ctx context.Context, c *api.Client, o chispaDeployOptions) error {
	name := o.Name
	model, err := chispa.Load(bytes.NewReader(o.Model))
	if err != nil {
		return fmt.Errorf("%s does not load as a .chispa: %w", o.ModelName, err)
	}
	modelSHA256 := sha256.Sum256(o.Model)
	spec := ChispaSpec{ModelB64: base64.StdEncoding.EncodeToString(o.Model)}
	rec := aigw.ChispaDeployRecord{Labels: model.Labels, Sha256: hex.EncodeToString(modelSHA256[:])}
	if len(o.Slots) > 0 {
		sm, err := slots.Load(bytes.NewReader(o.Slots))
		if err != nil {
			return fmt.Errorf("the slot model does not load as a .chispas: %w", err)
		}
		spec.SlotsB64 = base64.StdEncoding.EncodeToString(o.Slots)
		sum := sha256.Sum256(o.Slots)
		// Los huecos también van al registro: el gateway valida contra ellos
		// los que mande la réplica (pkg/aigw/chispaguest.go).
		rec.Slots, rec.SlotsSha256 = sm.SlotNames(), hex.EncodeToString(sum[:])
	}
	if o.WarmText == "" {
		o.WarmText = "hello world"
	}

	if !o.Replace {
		if snaps, err := c.Snapshots(ctx); err == nil {
			for _, s := range snaps {
				if s.Name == name {
					return fmt.Errorf("snapshot %q already exists (use -replace, or kling chispa rm %s)", name, name)
				}
			}
		}
	}

	if o.Reuse && o.Rebuild {
		return fmt.Errorf("-reuse-image and -rebuild exclude each other")
	}
	exists := false
	if imgs, err := c.Images(ctx); err == nil {
		for _, img := range imgs {
			exists = exists || img.Name == name
		}
	}
	switch {
	case o.Reuse && !exists:
		return fmt.Errorf("-reuse-image: there is no image %q on this daemon (kling images copy %s -from <linux host>)", name, name)
	case exists && !o.Rebuild && !o.Reuse:
		return fmt.Errorf("image %q already exists (use -rebuild to build it again with this model, or -reuse-image if it already has it)", name)
	}

	if o.Reuse {
		// Un daemon de macOS no construye imágenes (loop y chroot son de
		// Linux): la imagen se construye en un Linux y se trae. No hay forma
		// de mirar dentro qué .chispa lleva, así que se confía en quien lo
		// dice; el registro de despliegue de abajo graba el sha256 de -model,
		// y si no casa con el de la imagen la réplica contestará etiquetas que
		// el gateway rechaza (502), no decisiones equivocadas en silencio.
		fmt.Printf("Reusing image %q (it must carry %s)...\n", name, o.ModelName)
	} else {
		fmt.Printf("Building image %q (kling-chispa + %s)...\n", name, o.ModelName)
		specJSON, _ := json.Marshal(spec)
		if _, err := c.BuildImage(ctx, api.BuildImageRequest{Name: name, Base: "min", Builder: "chispa", Spec: specJSON}); err != nil {
			return err
		}
	}

	fmt.Printf("Making the golden snapshot (%d vCPU, %d MiB)...\n", o.VCPUs, o.MemMiB)
	t0 := time.Now()
	g, err := makeChispaGolden(ctx, c, chispaGoldenOptions{
		Image: name, Snapshot: name, VCPUs: o.VCPUs, MemMiB: o.MemMiB,
		AllowExec: o.AllowExec, Replace: o.Replace, Wait: o.Wait, WarmText: o.WarmText,
		Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Printf("✓ %s  golden snapshot of task %s  (%s of memory, %s in total)\n",
		g.Snapshot.Name, name, human(g.Snapshot.MemBytes), time.Since(t0).Round(time.Second))

	// El gateway no tiene el .chispa de origen a mano (puede vivir en otra
	// máquina, o el daemon estar al otro lado de un SSH) y, sobre todo, no
	// puede fiarse de las etiquetas que le mande el propio invitado
	// (pkg/aigw/chispaguest.go): se graban aquí, en el dorado, como la fuente de
	// verdad que classifyGuest valida contra la respuesta de la réplica.
	if _, err := c.SetAnnotation(ctx, name, aigw.ChispaDeployAnnotation, rec); err != nil {
		return fmt.Errorf("recording %s on the snapshot (needed by the gateway to trust replica answers): %w", aigw.ChispaDeployAnnotation, err)
	}
	return nil
}

func cmdChispaLs(args []string) error {
	fs := flag.NewFlagSet("chispa ls", flag.ExitOnError)
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
		if s.Labels[chispaLabelTask] != "" {
			tasks = append(tasks, s)
		}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(tasks)
	}
	if len(tasks) == 0 {
		fmt.Println("no chispa tasks deployed (kling chispa deploy <task> -model m.chispa)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "TASK\tSNAPSHOT\tMEMORY\tREPLICAS RUNNING")
	for _, s := range tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", s.Labels[chispaLabelTask], s.Name, human(s.MemBytes), s.Instances)
	}
	return tw.Flush()
}

func cmdChispaRm(args []string) error {
	fs := flag.NewFlagSet("chispa rm", flag.ExitOnError)
	host := hostFlag(fs)
	keepImage := fs.Bool("keep-image", false, "remove only the golden snapshot")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling chispa rm <task> [-keep-image]")
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

// chispaGoldenOptions es von.GoldenOptions recortado a lo que un dorado de Chispa
// necesita: sin prefijos ni Kind de VON, con el puerto y la ruta de salud de
// kling-chispa en vez de las de llama-server.
type chispaGoldenOptions struct {
	Image, Snapshot    string
	VCPUs, MemMiB      int
	AllowExec, Replace bool
	Wait               time.Duration
	WarmText           string
	Log                func(format string, args ...any)
}

type chispaGoldenResult struct {
	Snapshot *api.Snapshot
	BootMS   int64
	LoadMS   int64
}

// makeChispaGolden arranca una microVM de la imagen, espera a que kling-chispa
// conteste /healthz, la calienta con una clasificación de verdad (toca las
// páginas del modelo y del binario antes de congelar) y la congela como
// dorado. Es el mismo patrón que von.MakeGolden, pero sin nada de llama.cpp:
// Chispa no necesita gramática, caché de prompts ni prefijos, así que
// reescribirlo aquí, más corto, es más claro que forzarlo dentro de
// von.GoldenOptions.
func makeChispaGolden(ctx context.Context, c *api.Client, o chispaGoldenOptions) (*chispaGoldenResult, error) {
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
		Egress: "none", Labels: chispaLabels(o.Snapshot, nil), AllowExec: o.AllowExec,
	})
	if err != nil {
		return nil, fmt.Errorf("booting %s: %w", o.Image, err)
	}
	res := &chispaGoldenResult{BootMS: mc.BootMS}
	defer func() {
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = c.Remove(rc, mc.ID)
	}()
	logf("booted %s in %d ms; waiting for kling-chispa...", mc.Name, mc.BootMS)

	t0 := time.Now()
	if err := waitChispaHealthy(ctx, c, mc.ID, o.Wait); err != nil {
		return nil, fmt.Errorf("%w\nsee the service log with:  kling exec %s -- tail -50 /var/log/service.log", err, mc.Name)
	}
	res.LoadMS = time.Since(t0).Milliseconds()
	logf("kling-chispa ready in %s; warming up...", time.Since(t0).Round(time.Millisecond))

	body, _ := json.Marshal(map[string]any{"text": o.WarmText})
	resp, err := c.Guest(ctx, mc.ID, api.GuestRequest{
		Port: chispa.GuestPort, Path: "/v1/classify", Method: http.MethodPost, Body: string(body),
	})
	if err != nil {
		return nil, fmt.Errorf("warm-up: %w", err)
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("warm-up: kling-chispa answered %d: %s", resp.Status, resp.Body)
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

// waitChispaHealthy espera a que el puerto de kling-chispa abra y luego a que
// /healthz conteste 200: igual que von.WaitReady, pero contra el agente de
// Chispa en vez de llama-server.
func waitChispaHealthy(ctx context.Context, c *api.Client, ref string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if _, err := c.Guest(ctx, ref, api.GuestRequest{
		Port: chispa.GuestPort, WaitMS: int(timeout / time.Millisecond), ProbeOnly: true,
	}); err != nil {
		return fmt.Errorf("kling-chispa did not open port %d: %w", chispa.GuestPort, err)
	}
	var last error
	for time.Now().Before(deadline) {
		resp, err := c.Guest(ctx, ref, api.GuestRequest{Port: chispa.GuestPort, Path: "/healthz", Method: http.MethodGet})
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
	return fmt.Errorf("kling-chispa not ready after %s: %w", timeout, last)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
