package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/api"
)

// `kindling-domotica eval-llm`: la capa 4 (un LLM VON con salida JSON) frente a
// «escalar y no hacer nada» en lo que las capas rápidas escalan. Pregunta al
// LLM por el mismo camino que en producción, una tarea de generación del
// gateway de IA (POST /v1/generate) con el prompt y el esquema de
// internal/domotica:
//
//   - -gateway <socket|URL> -llm-task T [-decide-task D]: un `kindling-domotica gateway`
//     que ya corre (las capas 1–3 por /v1/decide si se da -decide-task; si no,
//     en el proceso).
//   - -von <dorado>: un gateway en el propio proceso sobre el daemon de -H,
//     con esa tarea creada al vuelo. Nada escucha en la red.
//
// El registro que escribe es la puerta de la capa 4: quien la use (la demo de
// examples/domotica) solo la enciende si la evaluación la respalda.

// llmTaskName es la tarea de generación del gateway en el proceso.
const llmTaskName = "room-llm"

// LLMTaskConfig es la tarea de generación de la capa 4 en el registro del
// gateway (ai.json): el prompt y el esquema de internal/domotica, temperatura 0.
func llmTaskConfig(model string) *aigw.TaskConfig {
	zero := 0.0
	return &aigw.TaskConfig{VON: model, System: domotica.LLMSystemPrompt, Prompt: "{input}",
		JSONSchema: domotica.LLMSchema, MaxTokens: 200, Temperature: &zero}
}

// llmConn es la capa 4 conectada y cómo cerrarla.
type llmConn struct {
	gw     *domotica.GatewayClient
	task   string
	id     string // clave del registro: el dorado o la tarea del gateway
	desc   string
	inproc *aigw.Gateway
	client *api.Client
	cancel context.CancelFunc
}

// memTransport lleva una petición al handler del gateway del proceso sin red.
type memTransport struct{ h http.Handler }

func (t memTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r2 := r.Clone(r.Context())
	r2.RequestURI = r.URL.RequestURI()
	r2.RemoteAddr = "127.0.0.1:0"
	t.h.ServeHTTP(rec, r2)
	return rec.Result(), nil
}

// gatewayClient abre un `kindling-domotica gateway` por socket Unix o URL, con el token de
// ai.token (o $KLING_AI_TOKEN) si lo hay.
func gatewayClient(addr string, timeout time.Duration) *domotica.GatewayClient {
	c := &domotica.GatewayClient{Base: strings.TrimRight(addr, "/"), Client: &http.Client{Timeout: timeout}}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		sock := addr
		c.Client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}
		c.Base = "http://ai"
	}
	if t, err := aiToken(aiDefault("ai.token")); err == nil {
		c.Token = t
	}
	return c
}

func connectLLM(ctx context.Context, host, golden, gateway, task string, timeout time.Duration) (*llmConn, error) {
	if gateway != "" {
		if task == "" {
			return nil, errors.New("-gateway needs -llm-task (the generation task of layer 4 in ai.json)")
		}
		return &llmConn{gw: gatewayClient(gateway, timeout), task: task, id: task, desc: "gateway " + gateway + ", task " + task}, nil
	}
	if golden == "" {
		return nil, errors.New("give -von <golden> (in-process gateway) or -gateway <addr> -llm-task <task>")
	}
	cfg := &aigw.Config{
		Models: map[string]*aigw.ModelConfig{golden: {Kind: aigw.KindVON, Snapshot: golden, MaxReplicas: 1}},
		Tasks:  map[string]*aigw.TaskConfig{llmTaskName: llmTaskConfig(golden)},
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &llmConn{task: llmTaskName, id: golden, client: api.NewClient(hostOf(host))}
	sn, err := c.client.Snapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("layer 4 needs a daemon: %w", err)
	}
	found := false
	for _, s := range sn {
		found = found || s.Name == golden
	}
	if !found {
		return nil, fmt.Errorf("the daemon at %s has no golden snapshot %q (kling ai model add %s -model qwen2.5-1.5b-instruct -quant q4_k_m)",
			c.client.Endpoint(), golden, golden)
	}
	g, err := aigw.New(aigw.Options{Client: c.client, Config: cfg, ID: "domotica-eval", NamePrefix: "dom-",
		Idle: 10 * time.Minute, MaxReplicas: 1, VONTimeout: timeout})
	if err != nil {
		return nil, err
	}
	gctx, cancel := context.WithCancel(context.Background())
	g.Start(gctx)
	c.inproc, c.cancel = g, cancel
	c.gw = &domotica.GatewayClient{Base: "http://ai", Client: &http.Client{Transport: memTransport{g.Handler("")}}}
	c.desc = "in-process gateway on " + c.client.Endpoint() + ", golden " + golden
	return c, nil
}

// Close congela la réplica del gateway del proceso y borra sus máquinas: la
// evaluación no deja nada corriendo ni en disco.
func (c *llmConn) Close() {
	if c == nil || c.inproc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c.inproc.Close(ctx)
	c.cancel()
	if ms, err := c.client.List(ctx); err == nil {
		for _, m := range ms {
			if m.Labels[aigw.LabelGateway] == "domotica-eval" {
				_ = c.client.Remove(ctx, m.ID)
			}
		}
	}
}

// ── registro de la evaluación (la puerta de la capa 4) ──────────────────────

// Layer4Record es lo que guarda `eval-llm` y lee la demo. La capa 4 solo se
// enciende con uno del mismo modelo (dorado o tarea), prompt y validación
// (PromptID) y capas rápidas (FastID): si cambian, lo que escala cambia y la
// evaluación ya no vale.
type layer4Record struct {
	ID         string               `json:"id"` // dorado (en el proceso) o tarea del gateway
	PromptID   string               `json:"prompt_id"`
	FastID     string               `json:"fast_id"`
	Data       string               `json:"data"` // sobre qué se decidió: MASSIVE (si se dio) o las frases de reto
	Scopes     map[string]scopeGate `json:"scopes"`
	LatencyP50 float64              `json:"latency_p50_ms"`
	LatencyP90 float64              `json:"latency_p90_ms"`
	At         time.Time            `json:"at"`
}

type scopeGate struct {
	Pass  bool             `json:"pass"`
	Why   string           `json:"why"`
	Win   int              `json:"win"`
	Loss  int              `json:"loss"`
	P     float64          `json:"p"`
	Total domotica.L4Total `json:"total"`
}

func layer4RecordPath(id string) string {
	return filepath.Join(domoticaModelsDir(), "layer4-"+id+".json")
}

// fastID resume las capas del proceso que deciden qué escala (el sha256 de
// sus modelos). Con -decide-task es domotica.FastID de la tarea del gateway.
func fastID(intentPath, slotsPath string) string {
	h := sha256.New()
	for _, p := range []string{resolveModel(intentPath, "intent.chispa"), resolveModel(slotsPath, "slots.chispas")} {
		if p == "" {
			h.Write([]byte("none\x00"))
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			h.Write([]byte("missing\x00"))
			continue
		}
		_, _ = io.Copy(h, io.LimitReader(f, 256<<20))
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func resolveModel(p, def string) string {
	if p != "" {
		return p
	}
	if d := domoticaModelsDir(); d != "" && regularFile(filepath.Join(d, def)) {
		return filepath.Join(d, def)
	}
	return ""
}

// ── kindling-domotica eval-llm ─────────────────────────────────────────────────

func cmdDomoticaEvalLLM(args []string) error {
	fs := flag.NewFlagSet("domotica eval-llm", flag.ExitOnError)
	host := hostFlag(fs)
	golden := fs.String("von", "", "VON golden for an in-process gateway on the daemon of -H")
	gateway := fs.String("gateway", "", "a running `kindling-domotica gateway` (socket path or http://host:port) instead")
	llmTask := fs.String("llm-task", "", "with -gateway: the generation task of layer 4 (see llm-task.json in examples/domotica)")
	decideTask := fs.String("decide-task", "", "with -gateway: the domotica task that serves layers 1-3 (default: in-process layers 1-2)")
	timeout := fs.Duration("von-timeout", 60*time.Second, "deadline of one layer-4 decision (includes waking the replica)")
	data := fs.String("data", "", "unified JSONL test data; its MASSIVE rows are used")
	intentPath := fs.String("intent", "", "intent model (.chispa), in-process layers")
	slotsPath := fs.String("slots", "", "slot model (.chispas), in-process layers")
	oosPer := fs.Int("oos-sample", 100, "out-of-scope MASSIVE rows sampled per language (deterministic); they are weighted back")
	challenge := fs.Bool("challenge", true, "also score the built-in challenge set")
	conc := fs.Int("concurrency", 1, "parallel requests to VON")
	nerr := fs.Int("errors", 0, "print this many layer-4 errors")
	save := fs.Bool("save", true, "write the eval record that enables layer 4")
	dump := fs.String("dump", "", "write every escalated row with VON's answer to this JSONL file")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	var rows []domotica.L4Row
	if *data != "" {
		all, _, err := readRowsFile(*data)
		if err != nil {
			return err
		}
		rows = append(rows, massiveL4Rows(all, *oosPer)...)
	}
	if *challenge {
		for _, r := range domotica.Challenge() {
			rows = append(rows, domotica.L4Row{Row: r, Group: "challenge/" + r.Class, Weight: 1})
		}
	}
	if len(rows) == 0 {
		return errors.New("nothing to evaluate: give -data and/or -challenge")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	lc, err := connectLLM(ctx, *host, *golden, *gateway, *llmTask, *timeout)
	if err != nil {
		return err
	}
	defer lc.Close()
	var fast domotica.FastFunc
	fid := fastID(*intentPath, *slotsPath)
	if *decideTask != "" {
		if *gateway == "" {
			return errors.New("-decide-task needs -gateway")
		}
		tasks, err := lc.gw.Tasks(ctx)
		if err != nil {
			return err
		}
		fid = ""
		for _, t := range tasks {
			if t.Name == *decideTask && t.Kind == "intent" {
				fid = domotica.FastID(t)
			}
		}
		if fid == "" {
			return fmt.Errorf("the gateway has no intent task %q", *decideTask)
		}
		fast = lc.gw.Fast(*decideTask)
	} else {
		d, err := loadDecider(*intentPath, *slotsPath)
		if err != nil {
			return err
		}
		fast = domotica.InProcess(d)
	}
	layer := &domotica.VON{Generate: lc.gw.Generator(lc.task), Timeout: *timeout}
	fmt.Printf("layer 4: %s (prompt %s)\n", lc.desc, domotica.PromptID())
	// Primera llamada aparte: mide el despertar (thaw o restore) y no lo mezcla
	// con la latencia en caliente.
	t0 := time.Now()
	if _, err := layer.Decide(ctx, "enciende la luz", "es"); err != nil {
		return fmt.Errorf("layer 4 first call: %w", err)
	}
	fmt.Printf("first call (wake + prompt): %s\n", time.Since(t0).Round(time.Millisecond))
	rep, err := domotica.EvaluateLayer4(ctx, fast, layer, rows, *conc)
	if err != nil {
		return err
	}
	domotica.WriteLayer4(os.Stdout, rep)
	if *dump != "" {
		if err := writeDump(*dump, rep.Rows); err != nil {
			return err
		}
	}
	fmt.Printf("\nVON latency (warm): p50 %.0f ms  p90 %.0f ms  p99 %.0f ms  (%d calls)\n",
		rep.Percentile(0.5), rep.Percentile(0.9), rep.Percentile(0.99), len(rep.Latency))
	if m, err := lc.gw.Metrics(ctx); err == nil {
		for k, w := range domotica.ParseWakes(m) {
			if w.N > 0 {
				fmt.Printf("replica wakes %s: %d (avg %.0f ms)\n", k, w.N, 1000*w.Sum/float64(w.N))
			}
		}
	}
	gates, decideOn := printGates(rep, *challenge)
	for i, e := range rep.Errors {
		if i >= *nerr {
			break
		}
		fmt.Println("  " + e)
	}
	if *save {
		rec := layer4Record{ID: lc.id, PromptID: rep.PromptID, FastID: fid, Data: decideOn, Scopes: gates,
			LatencyP50: rep.Percentile(0.5), LatencyP90: rep.Percentile(0.9), At: time.Now().UTC()}
		b, _ := json.MarshalIndent(rec, "", "  ")
		p := layer4RecordPath(lc.id)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Printf("eval record: %s\n", p)
	}
	return nil
}

func writeDump(path string, rows []domotica.L4Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, r := range rows {
		_ = enc.Encode(r)
	}
	return f.Close()
}

// printGates enseña la puerta por conjunto (MASSIVE decide; las frases de reto
// se enseñan) y alcance, y devuelve la del conjunto que decide.
func printGates(rep *domotica.Layer4Report, challenge bool) (map[string]scopeGate, string) {
	groupsWith := func(prefix string) []string {
		var out []string
		for n := range rep.Groups {
			if strings.HasPrefix(n, prefix) {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
	// MASSIVE: sus órdenes y la muestra de lo que no es de la habitación, con
	// su peso.
	sets := []struct {
		name   string
		groups []string
	}{{"MASSIVE", groupsWith("massive")}, {"challenge", groupsWith("challenge/")}}
	decideOn := ""
	gates := map[string]scopeGate{}
	fmt.Println()
	for _, set := range sets {
		if len(set.groups) == 0 {
			continue
		}
		if decideOn == "" {
			decideOn = set.name
		}
		for _, sc := range []string{domotica.ScopeAll, domotica.ScopeUncertain} {
			g := rep.Gate(set.groups, sc)
			t := g.Total
			verdict := "fails"
			if g.Pass {
				verdict = "PASSES"
			}
			fmt.Printf("%-9s scope %-9s cascade exact %.3f → %.3f; confident errors %.1f%% → %.1f%% (weighted n=%.0f); wins %d losses %d p=%.2g: %s\n",
				set.name, sc, t.WithoutExact/t.N, t.WithExact/t.N, 100*t.WithoutWrong/t.N, 100*t.WithWrong/t.N, t.N, g.Win, g.Loss, g.P, verdict)
			if set.name == decideOn {
				gates[sc] = scopeGate{Pass: g.Pass, Why: g.Why, Win: g.Win, Loss: g.Loss, P: g.P, Total: g.Total}
			}
		}
	}
	verdict := "DISABLED"
	switch {
	case gates[domotica.ScopeAll].Pass:
		verdict = "ENABLED for every escalation"
	case gates[domotica.ScopeUncertain].Pass:
		verdict = "ENABLED only where the fast model is unsure (not on its out-of-scope)"
	}
	fmt.Printf("\ngate on %s: layer 4 %s\n  all: %s\n  uncertain: %s\n", decideOn, verdict, gates[domotica.ScopeAll].Why, gates[domotica.ScopeUncertain].Why)
	return gates, decideOn
}

// massiveL4Rows: todas las órdenes de MASSIVE y una muestra determinista de lo
// que no es de la habitación, con el peso que la devuelve a su proporción.
func massiveL4Rows(all []domotica.Row, oosPer int) []domotica.L4Row {
	oos := map[string][]domotica.Row{}
	var out []domotica.L4Row
	for _, r := range all {
		if r.Source != "massive" {
			continue
		}
		if r.Intent != domotica.OutOfScope {
			out = append(out, domotica.L4Row{Row: r, Group: "massive/" + r.Lang, Weight: 1})
			continue
		}
		oos[r.Lang] = append(oos[r.Lang], r)
	}
	for _, lang := range []string{"en", "es"} {
		rs := oos[lang]
		if len(rs) == 0 {
			continue
		}
		// Muestra por hash del texto: estable entre ejecuciones y máquinas.
		sort.Slice(rs, func(i, j int) bool { return textHash(rs[i].Text) < textHash(rs[j].Text) })
		n := min(oosPer, len(rs))
		w := float64(len(rs)) / float64(max(n, 1))
		for _, r := range rs[:n] {
			out = append(out, domotica.L4Row{Row: r, Group: "massive-oos/" + lang, Weight: w})
		}
	}
	return out
}

func textHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}
