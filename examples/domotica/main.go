// Command domotica es la habitación de demo de kindling: una página web con
// luces, termostato, persianas, tele, altavoz, cerradura, alarma, ventilador
// y enchufe simulados, que se manejan con órdenes de voz (como texto) en
// español o inglés.
//
// No decide nada por sí misma: cada orden va al gateway de IA de kindling
// (`kling ai serve`), que la pasa por las capas —plantillas y el modelo
// rápido Chispa en su proceso, el codificador de frases y el LLM (VON) en
// microVMs que se descongelan con la orden y se congelan al quedarse
// ociosas— y la página enseña qué capa decidió, en cuánto, qué microVM tuvo
// que despertar y cuánta memoria usan. Ver README.md.
//
//	go run ./examples/domotica -gateway ~/.config/kling/ai.sock -H unix:///run/kling/kling.sock
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/examples/domotica/room"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/domotica"
	"github.com/juan52878911/kindling/pkg/jev"
	"github.com/juan52878911/kindling/pkg/jev/slots"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatal("domotica: ", err)
	}
}

type flags struct {
	listen, gateway, tokenFile, host string
	decideTask, llmTask, record      string
	force, offline                   bool
	intent, slotsPath                string
	timeout                          time.Duration
}

func parseFlags() flags {
	dir := filepath.Dir(config.Path())
	var f flags
	flag.StringVar(&f.listen, "listen", "127.0.0.1:8088", "address of the web page (loopback by default; 0.0.0.0:8088 to reach it from the LAN)")
	flag.StringVar(&f.gateway, "gateway", filepath.Join(dir, "ai.sock"), "the AI gateway: its socket or http://host:port")
	flag.StringVar(&f.tokenFile, "token-file", filepath.Join(dir, "ai.token"), "gateway token for http:// ($KLING_AI_TOKEN wins)")
	flag.StringVar(&f.host, "H", "", "kindling daemon (socket or ssh://) for the machines panel; empty = the configured one, \"none\" = no panel")
	flag.StringVar(&f.decideTask, "decide-task", "room", "the gateway's domotica task: layers 1-3 (POST /v1/decide)")
	flag.StringVar(&f.llmTask, "llm-task", "room-llm", "the gateway's generation task of layer 4 (POST /v1/generate); empty = no layer 4")
	flag.StringVar(&f.record, "layer4-record", "", "eval record that enables layer 4 (default: layer4-<llm-task>.json in the domotica models dir)")
	flag.BoolVar(&f.force, "layer4-force", false, "enable layer 4 for every escalation even without an eval record that backs it")
	flag.BoolVar(&f.offline, "offline", false, "no gateway: layers 1-2 in this process (-intent, -slots), the rest unavailable")
	flag.StringVar(&f.intent, "intent", "", "with -offline: intent model (.jev)")
	flag.StringVar(&f.slotsPath, "slots", "", "with -offline: slot model (.jevs)")
	flag.DurationVar(&f.timeout, "timeout", 60*time.Second, "deadline of one command (includes waking microVMs)")
	flag.Parse()
	return f
}

func run() error {
	f := parseFlags()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		opts room.Options
		err  error
	)
	if f.offline {
		opts, err = offline(f)
	} else {
		opts, err = viaGateway(ctx, f)
	}
	if err != nil {
		return err
	}
	opts.Presets = room.Presets
	opts.LoopbackOnly = scheduler.IsLoopback(f.listen)
	if f.host != "none" {
		host := f.host
		if cfg, err := config.Load(); err == nil {
			host = cfg.Host(f.host)
		}
		// "" es el socket local por defecto, como en el CLI.
		c := api.NewClient(host)
		opts.Machines = func(ctx context.Context) ([]room.Machine, error) { return machines(ctx, c) }
	}

	srv := room.NewServer(opts)
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10}
	go srv.Run(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	fmt.Printf("demo room on http://%s/\n", ln.Addr())
	for _, l := range opts.Layers {
		fmt.Printf("  %-8s %-11s %-8s %s\n", l.Name, l.Status, l.Where, l.Detail)
	}
	if !opts.LoopbackOnly {
		fmt.Println("  not on loopback: anyone on this network can drive the simulated devices (and wake the layers' microVMs)")
	}
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ── con el gateway ──────────────────────────────────────────────────────────

func gatewayClient(f flags) (*domotica.GatewayClient, error) {
	c := &domotica.GatewayClient{Base: strings.TrimRight(f.gateway, "/"), Client: &http.Client{Timeout: f.timeout}}
	if !strings.HasPrefix(f.gateway, "http://") && !strings.HasPrefix(f.gateway, "https://") {
		// Un socket Unix (0600): el propio usuario, sin token.
		sock := f.gateway
		c.Client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}
		c.Base = "http://ai"
	}
	tok, err := token(f.tokenFile)
	if err != nil {
		return nil, err
	}
	c.Token = tok
	return c, nil
}

// token: $KLING_AI_TOKEN o el fichero, que no puede leer nadie más.
func token(path string) (string, error) {
	if t := os.Getenv("KLING_AI_TOKEN"); t != "" {
		return t, nil
	}
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (%v): chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func viaGateway(ctx context.Context, f flags) (room.Options, error) {
	gw, err := gatewayClient(f)
	if err != nil {
		return room.Options{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tasks, err := gw.Tasks(tctx)
	if err != nil {
		return room.Options{}, fmt.Errorf("the gateway at %s does not answer (is `kling ai serve` running? or use -offline): %w", f.gateway, err)
	}
	var decide, llm *domotica.TaskInfo
	for i := range tasks {
		switch t := &tasks[i]; t.Name {
		case f.decideTask:
			decide = t
		case f.llmTask:
			llm = t
		}
	}
	if decide == nil || decide.Kind != "domotica" {
		return room.Options{}, fmt.Errorf("the gateway has no domotica task %q (see ai.json in examples/domotica)", f.decideTask)
	}
	// Qué modelo es de qué capa, para atribuir los despertares de /metrics.
	modelLayer := map[string]string{decide.JEV: domotica.LayerJEV}
	layers := []room.LayerInfo{
		{Name: domotica.LayerTemplate, Status: "on", Where: "gateway", Detail: fmt.Sprintf("%d templates, task %s", len(domotica.DemoTemplates), f.decideTask)},
		{Name: domotica.LayerJEV, Status: "on", Where: "gateway", Detail: "model " + decide.JEV},
	}
	enc := room.LayerInfo{Name: domotica.LayerEncoder, Status: "unavailable", Where: "microvm", Detail: "the task has no encoder"}
	hasEncoder := false
	if c := decide.Cascade; c != nil && c.To != "" {
		modelLayer[c.To] = domotica.LayerEncoder
		enc.Detail = "model " + c.To
		switch c.Status {
		case "on", "forced":
			enc.Status, hasEncoder = c.Status, true
		default:
			enc.Status, enc.Detail = "off", c.Reason
		}
	}
	layers = append(layers, enc)

	cas := &domotica.Cascade{Fast: gw.Fast(f.decideTask), HasEncoder: hasEncoder}
	l4 := room.LayerInfo{Name: domotica.LayerVON, Status: "unavailable", Where: "microvm", Detail: "no generation task " + f.llmTask}
	var layer4 domotica.Layer = domotica.Unavailable
	scope := ""
	if llm != nil && llm.Kind == "generate" {
		modelLayer[llm.VON] = domotica.LayerVON
		sc, why := backed(f, domotica.FastID(*decide))
		switch {
		case sc != "":
			scope, l4.Status, l4.Detail = sc, "on", fmt.Sprintf("model %s, scope %s: %s", llm.VON, sc, why)
		case f.force:
			scope, l4.Status, l4.Detail = domotica.ScopeAll, "forced", "model "+llm.VON+", forced without backing: "+why
		default:
			l4.Status, l4.Detail = "off", why
		}
		layer4 = domotica.Disabled
		if scope != "" {
			layer4 = &domotica.VON{Generate: gw.Generator(f.llmTask), Timeout: f.timeout}
		}
	}
	layers = append(layers, l4)
	cas.Slow = []domotica.NamedLayer{{Name: domotica.LayerVON, Layer: layer4,
		// Fuera de su alcance ni se llama al LLM: no se despierta su microVM por
		// una frase que no es de la habitación.
		Skip: func(prev domotica.Decision) bool { return scope != "" && !domotica.InScope(scope, prev.Reason) }}}

	decideFn := func(ctx context.Context, text, lang string) room.Decided {
		ctx, cancel := context.WithTimeout(ctx, f.timeout)
		defer cancel()
		before, _ := gw.Metrics(ctx)
		tr := cas.Decide(ctx, text, lang)
		after, err := gw.Metrics(ctx)
		d := room.Decided{Trace: tr}
		if err == nil {
			for _, w := range domotica.WakesBetween(domotica.ParseWakes(before), domotica.ParseWakes(after)) {
				l := modelLayer[w.Model]
				if l == "" {
					l = w.Model
				}
				d.Wakes = append(d.Wakes, room.LayerWake{Layer: l, Wake: w})
			}
		}
		return d
	}
	return room.Options{Decide: decideFn, Layers: layers}, nil
}

// layer4Record es lo que escribe `kling domotica eval-llm` (los campos que
// hacen falta aquí).
type layer4Record struct {
	ID       string `json:"id"`
	PromptID string `json:"prompt_id"`
	FastID   string `json:"fast_id"`
	Scopes   map[string]struct {
		Pass bool   `json:"pass"`
		Why  string `json:"why"`
	} `json:"scopes"`
}

// backed dice con qué alcance respalda su evaluación a la capa 4: el más
// amplio que pasó la puerta, o "" y por qué no. Tiene que ser del mismo
// prompt y validación (los de esta demo) y de las mismas capas rápidas.
func backed(f flags, fastID string) (string, string) {
	p := f.record
	if p == "" {
		p = filepath.Join(modelsDir(), "layer4-"+f.llmTask+".json")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "no eval record " + p + " (kling domotica eval-llm -gateway … -llm-task " + f.llmTask + " -decide-task " + f.decideTask + " -data test.jsonl)"
	}
	var r layer4Record
	if err := json.Unmarshal(b, &r); err != nil {
		return "", "unreadable eval record: " + err.Error()
	}
	switch {
	case r.ID != f.llmTask || r.PromptID != domotica.PromptID():
		return "", "the eval record is for another task, prompt or validation"
	case r.FastID != fastID:
		return "", "the eval record is for other fast layers (" + r.FastID + ")"
	}
	for _, sc := range []string{domotica.ScopeAll, domotica.ScopeUncertain} {
		if g, ok := r.Scopes[sc]; ok && g.Pass {
			return sc, g.Why
		}
	}
	return "", "its eval does not back it: " + r.Scopes[domotica.ScopeAll].Why
}

func modelsDir() string {
	if d := os.Getenv("KLING_DOMOTICA_MODELS"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "kindling", "domotica", "models")
}

// ── sin gateway ─────────────────────────────────────────────────────────────

// offline es el modo de reserva: capas 1–2 en el proceso, sin microVMs.
func offline(f flags) (room.Options, error) {
	m, err := domotica.NewMatcher(domotica.DemoTemplates)
	if err != nil {
		return room.Options{}, err
	}
	d := &domotica.Decider{Matcher: m}
	ip, sp := f.intent, f.slotsPath
	if ip == "" {
		ip = filepath.Join(modelsDir(), "intent.jev")
	}
	if sp == "" {
		sp = filepath.Join(modelsDir(), "slots.jevs")
	}
	jevInfo := room.LayerInfo{Name: domotica.LayerJEV, Status: "unavailable", Where: "process", Detail: "no " + ip}
	if im, err := jev.LoadFile(ip); err == nil {
		d.Intent = im
		jevInfo.Status, jevInfo.Detail = "on", ip
		if sm, err := slots.LoadFile(sp); err == nil {
			d.Slots = sm
		}
	} else if f.intent != "" {
		return room.Options{}, err
	}
	cas := &domotica.Cascade{Fast: domotica.InProcess(d), Slow: []domotica.NamedLayer{{Name: domotica.LayerVON, Layer: domotica.Unavailable}}}
	return room.Options{
		Decide: func(ctx context.Context, text, lang string) room.Decided {
			return room.Decided{Trace: cas.Decide(ctx, text, lang)}
		},
		Layers: []room.LayerInfo{
			{Name: domotica.LayerTemplate, Status: "on", Where: "process", Detail: fmt.Sprintf("%d templates", len(domotica.DemoTemplates))},
			jevInfo,
			{Name: domotica.LayerEncoder, Status: "unavailable", Where: "microvm", Detail: "offline mode"},
			{Name: domotica.LayerVON, Status: "unavailable", Where: "microvm", Detail: "offline mode"},
		},
	}, nil
}

// ── máquinas ────────────────────────────────────────────────────────────────

// machines lista las microVMs de las capas con su memoria, del daemon.
func machines(ctx context.Context, c *api.Client) ([]room.Machine, error) {
	ms, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	mem := map[string]int64{}
	if ps, err := c.ProcStats(ctx); err == nil {
		for _, p := range ps.Machines {
			mem[p.ID] = p.PSSMiB
		}
	}
	out := []room.Machine{}
	for _, m := range ms {
		l := layerOf(m)
		if l == "" {
			continue
		}
		out = append(out, room.Machine{Name: m.Name, Layer: l, State: string(m.State), MemMiB: mem[m.ID], From: m.From})
	}
	return out, nil
}

// layerOf reconoce la capa de una microVM por sus etiquetas: los dorados de
// VON llevan von.model, los codificadores además von.kind=embed, y los del
// modelo rápido servido como microVM se llaman chispa-* o jev-*.
func layerOf(m *api.Machine) string {
	switch {
	case m.Labels["von.kind"] == "embed":
		return domotica.LayerEncoder
	case m.Labels["von.model"] != "":
		return domotica.LayerVON
	}
	for _, s := range []string{m.Name, m.From, m.Labels[api.LabelService]} {
		if strings.HasPrefix(s, "chispa") || strings.HasPrefix(s, "jev") {
			return domotica.LayerJEV
		}
	}
	return ""
}
