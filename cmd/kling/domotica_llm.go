package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/domotica"
)

// La capa 4 (VON) desde la CLI: `kling domotica eval-llm` la evalúa contra
// «escalar y no hacer nada», `cascade` pasa una orden por la cascada entera y
// `demo` la usa. VON se alcanza de dos formas:
//
//   - -von <dorado>: un gateway de IA en el propio proceso (pkg/aigw) sobre el
//     daemon de -H. Despierta la réplica con la primera orden, la congela al
//     quedarse ociosa y al salir. Nada escucha en la red: el cliente HTTP le
//     habla al handler en memoria.
//   - -ai <socket|URL> -von <modelo>: un `kling ai serve` que ya corre.

type vonOpts struct {
	host    *string
	von     *string
	ai      *string
	idle    *time.Duration
	timeout *time.Duration
	force   *bool
}

func vonFlags(fs *flag.FlagSet) *vonOpts {
	return &vonOpts{
		host:    hostFlag(fs),
		von:     fs.String("von", "", "layer 4: VON golden snapshot (or the model name on -ai); empty = layer 4 unavailable"),
		ai:      fs.String("ai", "", "use a running `kling ai serve` (socket path or http://host:port) instead of an in-process gateway"),
		idle:    fs.Duration("von-idle", 2*time.Minute, "freeze the VON replica after this long without commands"),
		timeout: fs.Duration("von-timeout", 60*time.Second, "deadline of one layer-4 decision (includes waking the replica)"),
		force:   fs.Bool("von-force", false, "enable layer 4 even without an eval record that backs it"),
	}
}

// vonConn es la capa 4 conectada, con lo que la demo enseña de ella.
type vonConn struct {
	Layer   *domotica.VON
	Desc    string
	golden  string
	gw      *aigw.Gateway
	handler http.Handler
	client  *api.Client
	cancel  context.CancelFunc
}

// memTransport lleva una petición al handler del gateway sin red.
type memTransport struct{ h http.Handler }

func (t memTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r2 := r.Clone(r.Context())
	r2.RequestURI = r.URL.RequestURI()
	r2.RemoteAddr = "127.0.0.1:0"
	t.h.ServeHTTP(rec, r2)
	return rec.Result(), nil
}

func (o *vonOpts) connect(ctx context.Context) (*vonConn, error) {
	if *o.von == "" {
		return nil, nil
	}
	c := &vonConn{golden: *o.von}
	lay := &domotica.VON{Model: *o.von, Timeout: *o.timeout}
	if *o.ai != "" {
		hc := &http.Client{Timeout: *o.timeout + 5*time.Second}
		base := strings.TrimRight(*o.ai, "/")
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			sock := base
			hc.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			}}
			base = "http://ai"
		}
		if t, err := aiToken(aiDefault("ai.token"), false); err == nil {
			lay.Header = http.Header{"Authorization": {"Bearer " + t}}
		}
		lay.Endpoint, lay.Client = base+"/v1/chat/completions", hc
		c.Layer, c.Desc = lay, "gateway "+*o.ai+", model "+*o.von
		return c, nil
	}
	cfg := &aigw.Config{Models: map[string]*aigw.ModelConfig{
		*o.von: {Kind: aigw.KindVON, Snapshot: *o.von, MaxReplicas: 1},
	}}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c.client = api.NewClient(hostOf(*o.host))
	sn, err := c.client.Snapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("layer 4 needs a daemon: %w", err)
	}
	found := false
	for _, s := range sn {
		found = found || s.Name == *o.von
	}
	if !found {
		return nil, fmt.Errorf("the daemon at %s has no golden snapshot %q (kling models add %s -model qwen2.5-1.5b-instruct -quant q4_k_m)",
			c.client.Endpoint(), *o.von, *o.von)
	}
	g, err := aigw.New(aigw.Options{Client: c.client, Config: cfg, ID: "domotica", NamePrefix: "dom-",
		Idle: *o.idle, MaxReplicas: 1, VONTimeout: *o.timeout})
	if err != nil {
		return nil, err
	}
	gctx, cancel := context.WithCancel(context.Background())
	g.Start(gctx)
	c.gw, c.cancel, c.handler = g, cancel, g.Handler("")
	lay.Endpoint, lay.Client = "http://ai/v1/chat/completions", &http.Client{Transport: memTransport{c.handler}}
	c.Layer, c.Desc = lay, "in-process gateway on "+c.client.Endpoint()+", golden "+*o.von
	return c, nil
}

// Close congela la réplica y borra las máquinas del gateway del proceso: la
// demo no deja nada corriendo ni en disco.
func (c *vonConn) Close() {
	if c == nil || c.gw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c.gw.Close(ctx)
	c.cancel()
	if ms, err := c.client.List(ctx); err == nil {
		for _, m := range ms {
			if m.Labels[aigw.LabelGateway] == "domotica" {
				_ = c.client.Remove(ctx, m.ID)
			}
		}
	}
}

// VONStats es lo que la demo enseña del gateway: despertares y réplicas.
type VONStats struct {
	Thaws    int     `json:"thaws"`
	Restores int     `json:"restores"`
	WakeMS   float64 `json:"wake_ms_avg"`
	Running  int     `json:"running"`
	Warm     int     `json:"warm"`
	MemMiB   int64   `json:"mem_mib,omitempty"` // del daemon, si lo sabe medir
}

// Stats lee /metrics del gateway (en proceso o remoto) y /procstats del daemon.
func (c *vonConn) Stats(ctx context.Context) (*VONStats, error) {
	if c == nil {
		return nil, nil
	}
	var body []byte
	if c.handler != nil {
		rec := httptest.NewRecorder()
		c.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil).WithContext(ctx))
		body = rec.Body.Bytes()
	} else {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.Layer.Endpoint, "/v1/chat/completions")+"/metrics", nil)
		if err != nil {
			return nil, err
		}
		for k, v := range c.Layer.Header {
			req.Header[k] = v
		}
		resp, err := c.Layer.Client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
			return nil, err
		}
	}
	st := parseVONMetrics(body, c.golden)
	if c.client != nil {
		if ps, err := c.client.ProcStats(ctx); err == nil {
			for _, m := range ps.Machines {
				if m.From == c.golden {
					st.MemMiB += m.PSSMiB
				}
			}
		}
	}
	return st, nil
}

func parseVONMetrics(b []byte, model string) *VONStats {
	st := &VONStats{}
	var wakeSum float64
	q := `model="` + model + `"`
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, q) {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "kling_ai_von_wake_seconds_count") && strings.Contains(line, `how="thaw"`):
			st.Thaws = int(v)
		case strings.HasPrefix(line, "kling_ai_von_wake_seconds_count") && strings.Contains(line, `how="restore"`):
			st.Restores = int(v)
		case strings.HasPrefix(line, "kling_ai_von_wake_seconds_sum"):
			wakeSum += v
		case strings.HasPrefix(line, "kling_ai_von_replicas") && strings.Contains(line, `state="running"`):
			st.Running = int(v)
		case strings.HasPrefix(line, "kling_ai_von_replicas") && strings.Contains(line, `state="warm"`):
			st.Warm = int(v)
		}
	}
	if n := st.Thaws + st.Restores; n > 0 {
		st.WakeMS = 1000 * wakeSum / float64(n)
	}
	return st
}

// ── registro de la evaluación (la puerta de la capa 4) ──────────────────────

// layer4Record es lo que guarda `eval-llm`. La capa 4 solo se enciende si hay
// uno que la respalde para el MISMO dorado, prompt y modelos rápidos: si
// cambian, lo que escala cambia y la evaluación ya no vale.
type layer4Record struct {
	Golden     string               `json:"golden"`
	PromptID   string               `json:"prompt_id"`
	FastSHA    string               `json:"fast_sha256"`
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

func layer4RecordPath(golden string) string {
	return filepath.Join(domoticaModelsDir(), "layer4-"+golden+".json")
}

// fastSHA resume los modelos rápidos cargados (los que deciden qué escala).
func fastSHA(intentPath, slotsPath string) string {
	h := sha256.New()
	for _, p := range []string{resolveModel(intentPath, "intent.jev"), resolveModel(slotsPath, "slots.jevs")} {
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

// layer4Backed dice con qué alcance respalda su evaluación a la capa 4: el
// más amplio que pasó la puerta (all antes que uncertain), o "" y por qué no.
func layer4Backed(golden, intentPath, slotsPath string) (scope, why string) {
	b, err := os.ReadFile(layer4RecordPath(golden))
	if err != nil {
		return "", "no eval record (run `kling domotica eval-llm -von " + golden + " -data test.jsonl`)"
	}
	var r layer4Record
	if err := json.Unmarshal(b, &r); err != nil {
		return "", "unreadable eval record: " + err.Error()
	}
	switch {
	case r.Golden != golden || r.PromptID != domotica.PromptID():
		return "", "the eval record is for another golden or prompt"
	case r.FastSHA != fastSHA(intentPath, slotsPath):
		return "", "the eval record is for other fast-layer models"
	}
	for _, sc := range []string{domotica.ScopeAll, domotica.ScopeUncertain} {
		if g, ok := r.Scopes[sc]; ok && g.Pass {
			return sc, g.Why
		}
	}
	return "", "its eval does not back it: " + r.Scopes[domotica.ScopeAll].Why
}

// ── kling domotica eval-llm ─────────────────────────────────────────────────

func cmdDomoticaEvalLLM(args []string) error {
	fs := flag.NewFlagSet("domotica eval-llm", flag.ExitOnError)
	vo := vonFlags(fs)
	data := fs.String("data", "", "unified JSONL test data; its MASSIVE rows are used")
	intentPath := fs.String("intent", "", "intent model (.jev)")
	slotsPath := fs.String("slots", "", "slot model (.jevs)")
	oosPer := fs.Int("oos-sample", 100, "out-of-scope MASSIVE rows sampled per language (deterministic); they are weighted back")
	challenge := fs.Bool("challenge", true, "also score the built-in challenge set")
	conc := fs.Int("concurrency", 1, "parallel requests to VON")
	nerr := fs.Int("errors", 0, "print this many layer-4 errors")
	save := fs.Bool("save", true, "write the eval record that enables layer 4 in the demo")
	dump := fs.String("dump", "", "write every escalated row with VON's answer to this JSONL file")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if *vo.von == "" {
		return errors.New("-von <golden> is required")
	}
	d, err := loadDecider(*intentPath, *slotsPath)
	if err != nil {
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
	vc, err := vo.connect(ctx)
	if err != nil {
		return err
	}
	defer vc.Close()
	fmt.Printf("layer 4: %s (prompt %s)\n", vc.Desc, domotica.PromptID())
	// Primera llamada aparte: mide el despertar (thaw o restore) y no lo mezcla
	// con la latencia en caliente.
	t0 := time.Now()
	if _, err := vc.Layer.Decide(ctx, "enciende la luz", "es"); err != nil {
		return fmt.Errorf("layer 4 first call: %w", err)
	}
	fmt.Printf("first call (wake + prompt): %s\n", time.Since(t0).Round(time.Millisecond))
	rep := domotica.EvaluateLayer4(ctx, d, vc.Layer, rows, *conc)
	domotica.WriteLayer4(os.Stdout, rep)
	if *dump != "" {
		f, err := os.Create(*dump)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		for _, r := range rep.Rows {
			_ = enc.Encode(r)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	fmt.Printf("\nVON latency (warm): p50 %.0f ms  p90 %.0f ms  p99 %.0f ms  (%d calls)\n",
		rep.Percentile(0.5), rep.Percentile(0.9), rep.Percentile(0.99), len(rep.Latency))
	if st, err := vc.Stats(ctx); err == nil && st != nil {
		fmt.Printf("replica wakes: %d restore, %d thaw (avg %.0f ms)\n", st.Restores, st.Thaws, st.WakeMS)
	}
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
	// su peso. Es el conjunto que decide; las frases de reto se enseñan.
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
		verdict = "ENABLED only where JEV is unsure (not on its out-of-scope)"
	}
	fmt.Printf("\ngate on %s: layer 4 %s\n  all: %s\n  uncertain: %s\n", decideOn, verdict, gates[domotica.ScopeAll].Why, gates[domotica.ScopeUncertain].Why)
	for i, e := range rep.Errors {
		if i >= *nerr {
			break
		}
		fmt.Println("  " + e)
	}
	if *save && *vo.ai == "" {
		rec := layer4Record{Golden: *vo.von, PromptID: rep.PromptID, FastSHA: fastSHA(*intentPath, *slotsPath), Data: decideOn, Scopes: gates,
			LatencyP50: rep.Percentile(0.5), LatencyP90: rep.Percentile(0.9), At: time.Now().UTC()}
		b, _ := json.MarshalIndent(rec, "", "  ")
		p := layer4RecordPath(*vo.von)
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

// massiveL4Rows: todas las órdenes de MASSIVE y una muestra determinista de lo
// que no es de la habitación, con el peso que la devuelve a su proporción.
func massiveL4Rows(all []domotica.Row, oosPer int) []domotica.L4Row {
	type key struct{ lang string }
	oos := map[key][]domotica.Row{}
	var out []domotica.L4Row
	for _, r := range all {
		if r.Source != "massive" {
			continue
		}
		if r.Intent != domotica.OutOfScope {
			out = append(out, domotica.L4Row{Row: r, Group: "massive/" + r.Lang, Weight: 1})
			continue
		}
		oos[key{r.Lang}] = append(oos[key{r.Lang}], r)
	}
	for k, rs := range oos {
		// Muestra por hash del texto: estable entre ejecuciones y máquinas.
		sort.Slice(rs, func(i, j int) bool { return textHash(rs[i].Text) < textHash(rs[j].Text) })
		n := min(oosPer, len(rs))
		w := float64(len(rs)) / float64(max(n, 1))
		for _, r := range rs[:n] {
			out = append(out, domotica.L4Row{Row: r, Group: "massive-oos/" + k.lang, Weight: w})
		}
	}
	return out
}

func textHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// ── kling domotica cascade ──────────────────────────────────────────────────

func cmdDomoticaCascade(args []string) error {
	fs := flag.NewFlagSet("domotica cascade", flag.ExitOnError)
	vo := vonFlags(fs)
	lang := fs.String("lang", "auto", "language: es, en or auto")
	intentPath := fs.String("intent", "", "intent model (.jev)")
	slotsPath := fs.String("slots", "", "slot model (.jevs)")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: kling domotica cascade [-von golden] \"<text>\"")
	}
	d, err := loadDecider(*intentPath, *slotsPath)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	vc, err := vo.connect(ctx)
	if err != nil {
		return err
	}
	defer vc.Close()
	c := buildCascade(d, vc, *vo.force, *intentPath, *slotsPath)
	tr := c.cascade.Decide(ctx, strings.Join(fs.Args(), " "), *lang)
	b, _ := json.MarshalIndent(tr, "", "  ")
	fmt.Println(string(b))
	if c.note != "" {
		fmt.Fprintln(os.Stderr, c.note)
	}
	return nil
}

type builtCascade struct {
	cascade *domotica.Cascade
	von     string // "on", "forced", "off: <por qué>", "unavailable"
	scope   string // alcance de la capa 4 cuando está encendida
	note    string
}

// buildCascade monta la cascada: capas rápidas → codificador (hueco hasta que
// llegue el real) → VON si está conectado y respaldado por su evaluación, con
// el alcance que la evaluación respalda. -von-force la enciende para todo.
func buildCascade(d *domotica.Decider, vc *vonConn, force bool, intentPath, slotsPath string) builtCascade {
	b := builtCascade{cascade: &domotica.Cascade{Fast: d}, von: "unavailable"}
	b.cascade.Slow = append(b.cascade.Slow, domotica.NamedLayer{
		Name: domotica.LayerEncoder, Layer: domotica.Unavailable,
		// El codificador devuelve una sola intención: las órdenes múltiples
		// van directas a la capa que sabe devolver varias.
		Skip: func(prev domotica.Decision) bool { return prev.Reason == domotica.ReasonMultiCommand },
	})
	var l4 domotica.Layer = domotica.Unavailable
	if vc != nil {
		scope, why := layer4Backed(vc.golden, intentPath, slotsPath)
		switch {
		case scope != "":
			b.von, b.scope, b.note = "on", scope, "layer 4 enabled ("+scope+"): "+why
		case force:
			b.von, b.scope, b.note = "forced", domotica.ScopeAll, "layer 4 FORCED without backing: "+why
		default:
			b.von, b.note = "off: "+why, "layer 4 disabled: "+why+" (-von-force to enable it anyway)"
		}
		l4 = domotica.Disabled
		if b.scope != "" {
			l4 = vc.Layer
		}
	}
	sc := b.scope
	b.cascade.Slow = append(b.cascade.Slow, domotica.NamedLayer{Name: domotica.LayerVON, Layer: l4,
		// Fuera de su alcance ni se llama al LLM: no cuesta segundos ni despierta
		// la réplica por una frase que no es de la habitación.
		Skip: func(prev domotica.Decision) bool { return sc != "" && !domotica.InScope(sc, prev.Reason) }})
	return b
}
