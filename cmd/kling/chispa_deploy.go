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

	"github.com/juan52878911/kindling/internal/aigw"
	"github.com/juan52878911/kindling/internal/chispa"
	"github.com/juan52878911/kindling/internal/chispa/slots"
	"github.com/juan52878911/kindling/internal/dorado"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/units"
)

// `kling ai chispa deploy|ls|rm`: Chispa como tarea serverless de kindling
// (docs/chispa-serverless.md), la alternativa a servirlo en proceso dentro de
// `kling ai serve`. Cada tarea es su PROPIA imagen (el .chispa va horneado
// dentro, como el GGUF de un modelo VON) y su propio dorado congelado: el
// planificador la despierta con la primera petición y la congela al quedarse
// ociosa, exactamente como a un VON. No hace falta ni entiende de llama.cpp:
// el invitado es kling-chispa (cmd/kling-chispa), un binario estático que solo sabe
// de Chispa.

// LabelTask marca el dorado y sus réplicas con el nombre de la tarea, para
// `kling ai chispa ls` y para que un humano que mire `kling ps` sepa qué es.
const chispaLabelTask = "chispa.task"

func chispaLabels(name string, extra map[string]string) map[string]string {
	return api.MergeLabels(extra, map[string]string{
		chispaLabelTask:  name,
		api.LabelPorts:   strconv.Itoa(chispa.GuestPort),
		api.LabelService: name,
	})
}

func cmdChispaDeploy(args []string) error {
	fs := flag.NewFlagSet("chispa deploy", flag.ContinueOnError)
	host := hostFlag(fs)
	modelPath := fs.String("model", "", "the .chispa to deploy (required)")
	slotsPath := fs.String("slots", "", "optional .chispas (slots, docs/intent.md)")
	// 128 y no 64: un modelo de 28 etiquetas tardaba 1,96 s en calentar y, tras
	// el thaw, rechazaba conexiones con 64 MiB (se quedaba corto de memoria).
	mem := units.MiBVar(fs, "mem", 128, "microVM memory: 64M, 128M (bare number = MiB; 64-128 is usually enough; see docs/chispa-serverless.md)")
	cpus := fs.Int("cpus", 1, "microVM vCPUs")
	flagRetirado(fs, "vcpus", "cpus")
	warmText := fs.String("warm-text", "hello world", "text sent once before freezing, to touch the model's pages")
	replace := fs.Bool("replace", false, "replace the golden snapshot if it exists")
	rebuild := fs.Bool("rebuild", false, "rebuild the image even if one with this name exists")
	reuse := fs.Bool("reuse-image", false, "the image already exists with this same -model (built on a Linux host and brought with `kling image copy`): make only the golden snapshot")
	allowExec := fs.Bool("allow-exec", false, "keep kling exec/cp working in this task's machines (debugging)")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for kling-chispa to answer /healthz")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *modelPath == "" {
		return errUso("usage: kling ai chispa deploy <task> -model m.chispa [-slots s.chispas] [-mem 128M] [-cpus 1]")
	}
	vcpus := cpus
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

// chispaDeployOptions es lo que necesita un despliegue: lo usan `kling ai chispa
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
		// los que mande la réplica (internal/aigw/chispaguest.go).
		rec.Slots, rec.SlotsSha256 = sm.SlotNames(), hex.EncodeToString(sum[:])
	}
	if o.WarmText == "" {
		o.WarmText = "hello world"
	}

	if !o.Replace {
		if snaps, err := c.Snapshots(ctx); err == nil {
			for _, s := range snaps {
				if s.Name == name {
					return fmt.Errorf("snapshot %q already exists (use -replace, or kling ai chispa rm %s)", name, name)
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
		return fmt.Errorf("-reuse-image: there is no image %q on this daemon (kling image copy %s -from <linux host>)", name, name)
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
	fmt.Printf(uiMarks().ok+" %s  template of task %s  (%s of memory, %s in total)\n",
		g.Snapshot.Name, name, human(g.Snapshot.MemBytes), time.Since(t0).Round(time.Second))
	next("kling run -from %s", name)

	// El gateway no tiene el .chispa de origen a mano (puede vivir en otra
	// máquina, o el daemon estar al otro lado de un SSH) y, sobre todo, no
	// puede fiarse de las etiquetas que le mande el propio invitado
	// (internal/aigw/chispaguest.go): se graban aquí, en el dorado, como la fuente de
	// verdad que classifyGuest valida contra la respuesta de la réplica.
	if _, err := c.SetAnnotation(ctx, name, aigw.ChispaDeployAnnotation, rec); err != nil {
		return fmt.Errorf("recording %s on the snapshot (needed by the gateway to trust replica answers): %w", aigw.ChispaDeployAnnotation, err)
	}
	return nil
}

func cmdChispaLs(args []string) error {
	fs := flag.NewFlagSet("chispa ls", flag.ContinueOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
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
		fmt.Println("no chispa tasks deployed (kling ai chispa deploy <task> -model m.chispa)")
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
	fs := flag.NewFlagSet("chispa rm", flag.ContinueOnError)
	host := hostFlag(fs)
	keepImage := fs.Bool("keep-image", false, "remove only the golden snapshot")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUso("usage: kling ai chispa rm <task> [-keep-image]")
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

// chispaGoldenOptions es lo que un dorado de Chispa necesita del fabricante
// (internal/dorado): el puerto, la sonda y el calentamiento son los de
// kling-chispa y no se eligen.
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
// dorado. Es el fabricante genérico (internal/dorado) con el puerto, la sonda y
// el calentamiento de kling-chispa; como él, graba cpu_pct = 100 por vCPU
// (antes heredaba el 50 % del daemon, R8-10).
func makeChispaGolden(ctx context.Context, c *api.Client, o chispaGoldenOptions) (*chispaGoldenResult, error) {
	if o.Wait == 0 {
		o.Wait = 2 * time.Minute
	}
	body, _ := json.Marshal(map[string]any{"text": o.WarmText})
	g, err := dorado.Make(ctx, c, dorado.Options{
		Image: o.Image, Snapshot: o.Snapshot, Port: chispa.GuestPort,
		VCPUs: o.VCPUs, MemMiB: o.MemMiB,
		Health:    dorado.Probe{Path: "/healthz"},
		Warmup:    []dorado.Request{{Method: http.MethodPost, Path: "/v1/classify", Body: body}},
		Labels:    chispaLabels(o.Snapshot, nil),
		AllowExec: o.AllowExec, Replace: o.Replace, Wait: o.Wait, Log: o.Log,
	})
	if err != nil {
		return nil, err
	}
	return &chispaGoldenResult{Snapshot: g.Snapshot, BootMS: g.BootMS, LoadMS: g.LoadTime.Milliseconds()}, nil
}
