package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/aigw"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/scheduler"
	"github.com/juan52878911/kindling/pkg/von"
)

// GATEWAY DE IA: `kling ai`.
//
// Un proceso aparte del daemon, como el gateway MCP: el daemon nunca escucha
// en la red. Por defecto escucha en un socket Unix 0600 junto a la
// configuración (quien puede abrirlo es el propio usuario: no hace falta
// token); en TCP solo con -listen explícito y siempre con token. Diseño y
// cifras en docs/ai-gateway.md.

func cmdAI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kling ai <serve|ls|test|generate|eval|calibrate|reload|prime> [...]")
	}
	switch args[0] {
	case "serve":
		return aiServe(args[1:])
	case "ls", "list":
		return aiList(args[1:])
	case "test":
		return aiTest(args[1:])
	case "generate", "gen":
		return aiGenerate(args[1:])
	case "eval":
		return aiEval(args[1:])
	case "calibrate":
		return aiCalibrate(args[1:])
	case "reload":
		return aiReload(args[1:])
	case "prime":
		return aiPrime(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q: use serve, ls, test, generate, eval, calibrate, reload or prime", args[0])
	}
}

// aiDir es donde viven ai.json, ai.sock y ai.token: junto a config.json.
func aiDir() string { return filepath.Dir(config.Path()) }

func aiDefault(name string) string { return filepath.Join(aiDir(), name) }

func aiServe(args []string) error {
	fs := flag.NewFlagSet("ai serve", flag.ExitOnError)
	host := hostFlag(fs)
	cfgPath := fs.String("config", aiDefault("ai.json"), "model and task registry")
	socket := fs.String("socket", envOr("KLING_AI_SOCKET", aiDefault("ai.sock")), "Unix socket to listen on (mode 0600)")
	listen := fs.String("listen", "", "TCP address instead of the socket (requires a token)")
	tokenFile := fs.String("token-file", aiDefault("ai.token"), "token for -listen (generated 0600 if missing; $KLING_AI_TOKEN wins)")
	noAuth := fs.Bool("no-auth", false, "no token on -listen; development only, loopback only")
	id := fs.String("id", "default", "gateway id: label of its machines (ai.gateway=<id>)")
	namePrefix := fs.String("name-prefix", "gw-", "prefix of the replica machines' names")
	idle := fs.Duration("idle", 2*time.Minute, "time without requests before a replica is frozen")
	maxReplicas := fs.Int("max-replicas", 2, "replicas per model (max_replicas in the registry wins)")
	maxInflight := fs.Int("max-inflight", 1, "requests per replica before asking for another")
	keepwarm := fs.Int("keepwarm", 0, "N most used models kept awake (0 = pure scale to zero)")
	pausedMiB := fs.Int("paused-mib", 256, "MiB of idle replicas kept paused instead of frozen, small and popular first (0 = always freeze; docs/despertar.md)")
	pausedFor := fs.Duration("paused-for", 0, "how long a paused replica waits before it is frozen (0 = 10 × -idle)")
	chispaMem := fs.Int("chispa-mem", 256, "MiB of Chispa models kept loaded (LRU)")
	vonTimeout := fs.Duration("von-timeout", 60*time.Second, "deadline of an escalation to VON")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}

	// Primero lo que puede fallar sin hablar con nadie.
	var token string
	if *listen != "" {
		if *noAuth {
			if !scheduler.IsLoopback(*listen) {
				return fmt.Errorf("-no-auth requires listening on loopback, and %q is not: waking a model runs code", *listen)
			}
		} else {
			t, err := aiToken(*tokenFile, true)
			if err != nil {
				return err
			}
			token = t
		}
	}
	cfg, err := aigw.LoadConfig(*cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w\nwrite the registry first; see docs/ai-gateway.md for an example", err)
		}
		return err
	}
	if len(cfg.Tenants) > 0 {
		if st, err := os.Stat(*cfgPath); err == nil && st.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%s holds tenant tokens and is readable by others (%v): chmod 600 it", *cfgPath, st.Mode().Perm())
		}
		if token == "" && !*noAuth {
			// Con tenants hay que autenticar también en el socket, y el
			// principal (el que administra) es el del fichero de token.
			if token, err = aiToken(*tokenFile, true); err != nil {
				return err
			}
		}
	}

	ctx, stop := ctxWithSignals()
	defer stop()
	client := api.NewClient(hostOf(*host))
	g, err := aigw.New(aigw.Options{
		Client: client, ConfigPath: *cfgPath, Config: cfg, ID: *id, NamePrefix: *namePrefix,
		Idle: *idle, MaxReplicas: *maxReplicas, MaxInflight: *maxInflight, KeepWarm: *keepwarm,
		PausedMiB: *pausedMiB, PausedFor: *pausedFor,
		ChispaBudget: int64(*chispaMem) << 20, VONTimeout: *vonTimeout,
		PopularityFile: aiDefault("ai-popularity-" + *id + ".json"),
	})
	if err != nil {
		return err
	}

	var ln net.Listener
	where := *listen
	if *listen != "" {
		if ln, err = net.Listen("tcp", *listen); err != nil {
			return err
		}
	} else {
		if ln, err = listenUnix0600(*socket); err != nil {
			return err
		}
		where = *socket
		defer os.Remove(*socket)
	}

	g.Start(ctx)
	srv := &http.Server{
		Handler:           g.Handler(token),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	// SIGHUP relee el registro sin cortar nada.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			notes, err := g.Reload()
			if err != nil {
				log.Printf("reload: %v", err)
				continue
			}
			log.Printf("registry reloaded")
			for _, n := range notes {
				log.Print(n)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	auth := "no token (socket 0600)"
	switch {
	case token != "" && os.Getenv("KLING_AI_TOKEN") != "":
		auth = "token from $KLING_AI_TOKEN"
	case token != "":
		auth = "token in " + *tokenFile
	case *listen != "":
		auth = "NO AUTH (-no-auth)"
	}
	fmt.Printf("AI gateway on %s (%s); daemon %s\n", where, auth, client.Endpoint())
	fmt.Printf("  %d model(s), %d task(s); idle freeze after %s, up to %d replica(s) per model\n",
		len(cfg.Models), len(cfg.Tasks), *idle, *maxReplicas)
	for _, n := range aigw.CascadeNotes(g.Cascades()) {
		fmt.Println("  " + n)
	}
	err = srv.Serve(ln)
	// Al salir no queda ninguna réplica corriendo: se congelan todas.
	cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g.Close(cctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// listenUnix0600 escucha en un socket Unix que solo el usuario puede abrir. El
// directorio se crea 0700 y un socket viejo (de un gateway que murió) se
// retira solo si nadie contesta en él.
func listenUnix0600(path string) (net.Listener, error) {
	if len(path) >= 104 {
		return nil, fmt.Errorf("socket path %q is %d bytes; macOS allows 103: use -socket with a shorter path", path, len(path))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another gateway is already listening on %s", path)
		}
		_ = os.Remove(path)
	}
	old := syscall.Umask(0o177) // el socket nace 0600: sin ventana en la que otro lo abra
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(path, 0o600)
}

// aiToken devuelve el token del gateway: $KLING_AI_TOKEN, o el fichero (que
// tiene que ser 0600), o uno nuevo que se guarda 0600 si create. Nunca por la
// línea de comandos: cualquier usuario del host la lee en /proc.
func aiToken(path string, create bool) (string, error) {
	if t := os.Getenv("KLING_AI_TOKEN"); t != "" {
		return t, nil
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		st, serr := os.Stat(path)
		if serr == nil && st.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s is readable by others (%v): chmod 600 it", path, st.Mode().Perm())
		}
		t := strings.TrimSpace(string(b))
		if len(t) < 16 {
			return "", fmt.Errorf("%s: token too short", path)
		}
		return t, nil
	case !errors.Is(err, os.ErrNotExist) || !create:
		return "", err
	}
	t, err := scheduler.NewToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(t + "\n"); err != nil {
		return "", err
	}
	fmt.Printf("token generated in %s (mode 0600); clients send it as Authorization: Bearer <token>\n", path)
	return t, nil
}

// ---- cliente

// aiClient habla con un gateway: por su socket o, con -addr, por TCP con el
// token de $KLING_AI_TOKEN o del fichero de token.
type aiClient struct {
	base  string
	token string
	http  *http.Client
}

func aiClientFlags(fs *flag.FlagSet) func() (*aiClient, error) {
	socket := fs.String("socket", envOr("KLING_AI_SOCKET", aiDefault("ai.sock")), "gateway socket")
	addr := fs.String("addr", "", "gateway URL instead of the socket (http://host:port)")
	tokenFile := fs.String("token-file", aiDefault("ai.token"), "token for -addr (or $KLING_AI_TOKEN)")
	return func() (*aiClient, error) {
		c := &aiClient{http: &http.Client{Timeout: 5 * time.Minute}}
		// El token se manda si hay (un socket con tenants lo pide); si no hay
		// fichero ni variable, se va sin él.
		if t, err := aiToken(*tokenFile, false); err == nil {
			c.token = t
		} else if *addr != "" && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if *addr != "" {
			c.base = strings.TrimRight(*addr, "/")
			return c, nil
		}
		sock := *socket
		c.base = "http://ai"
		c.http.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}
		return c, nil
	}
}

func (c *aiClient) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gateway not reachable (is `kling ai serve` running?): %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s (%d)", e.Error.Message, resp.StatusCode)
		}
		return fmt.Errorf("gateway answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func aiList(args []string) error {
	fs := flag.NewFlagSet("ai ls", flag.ExitOnError)
	cfgPath := fs.String("config", aiDefault("ai.json"), "model and task registry")
	asJSON := fs.Bool("json", false, "JSON output")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	cfg, err := aigw.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	// Lo que sabe el gateway en marcha (modelos cargados, muestras), si lo hay.
	var live struct {
		Tasks []aigw.TaskInfo `json:"tasks"`
	}
	c, err := mk()
	running := err == nil && c.do(http.MethodGet, "/v1/tasks", nil, &live) == nil
	byName := map[string]aigw.TaskInfo{}
	for _, t := range live.Tasks {
		byName[t.Name] = t
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"models": cfg.Models, "tasks": cfg.Tasks, "running": running, "live": live.Tasks})
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tKIND\tSOURCE\tMAX REPLICAS")
	for _, n := range sortedNames(cfg.Models) {
		m := cfg.Models[n]
		src, reps := m.Path, "-"
		if m.Kind == aigw.KindVON || m.Kind == aigw.KindEmbed {
			src = m.Snapshot
			reps = "default"
			if m.MaxReplicas > 0 {
				reps = fmt.Sprint(m.MaxReplicas)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, m.Kind, src, reps)
	}
	fmt.Fprintln(tw, "\nTASK\tKIND\tMODEL\tCASCADE\tSAMPLES")
	var notes []string
	for _, n := range sortedNames(cfg.Tasks) {
		t := cfg.Tasks[n]
		samples, kind, model, casc := "-", "classify", t.Chispa, "-"
		if t.Domotica != nil {
			kind, model = "domotica", t.Domotica.Intent
			if t.Domotica.Encoder != "" {
				casc = "-> " + t.Domotica.Encoder
			}
		} else if t.IsGenerate() {
			kind, model = "generate", t.VON
		} else if t.EscalateTo != "" {
			casc = "-> " + t.EscalateTo
		}
		if lt, ok := byName[n]; ok {
			samples = fmt.Sprint(lt.Samples)
			if lt.Cascade != nil && lt.Cascade.Status != "off" {
				casc = lt.Cascade.Status + " -> " + lt.Cascade.To
				if lt.Cascade.Reason != "" {
					notes = append(notes, fmt.Sprintf("task %s: %s", n, lt.Cascade.Reason))
				}
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n, kind, model, casc, samples)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	if !running {
		fmt.Println("\n(gateway not running: start it with kling ai serve)")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func aiTest(args []string) error {
	fs := flag.NewFlagSet("ai test", flag.ExitOnError)
	mode := fs.String("mode", "", "cascade (default: Chispa, and VON where it is unsure if the task's cascade is on) or chispa")
	fields := fs.String("fields", "", `structured fields as JSON, e.g. {"service":"api"}`)
	asJSON := fs.Bool("json", false, "print the full JSON answer")
	explain := fs.Bool("explain", false, "include Chispa's evidence also when it answers")
	fs.String("lang", "", "language of a domotica command (es, en; default auto)")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling ai test <task> [-mode M] [-fields JSON] <text...>")
	}
	lang := fs.Lookup("lang").Value.String()
	req := aigw.ClassifyRequest{Task: fs.Arg(0), Text: strings.Join(fs.Args()[1:], " "), Mode: *mode, Explain: *explain}
	if *fields != "" {
		if err := json.Unmarshal([]byte(*fields), &req.Fields); err != nil {
			return fmt.Errorf("-fields: %w", err)
		}
	}
	c, err := mk()
	if err != nil {
		return err
	}
	if isDomoticaTask(c, req.Task) {
		req.Lang = lang
		var d aigw.DecideResponse
		if err := c.do(http.MethodPost, "/v1/decide", req, &d); err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(d)
		}
		sl, _ := json.Marshal(d.Slots)
		status := "confident"
		if !d.Confident {
			status = "escalate → " + d.Escalate + " (" + d.Reason + ")"
		}
		fmt.Printf("%s %s  (layer %s, p=%.3f, %s, %.2f ms)\n", orDash(d.Intent), sl, d.Layer, d.Prob, status, d.LatencyMS)
		if d.FastIntent != "" {
			fmt.Printf("  fast layers said %s p=%.3f; encoder %.1f ms\n", d.FastIntent, d.FastProb, d.EncoderUS/1000)
		}
		if d.EncoderError != "" {
			fmt.Printf("  encoder error: %s\n", d.EncoderError)
		}
		if d.Encoder != nil && d.Encoder.Status != "on" {
			fmt.Printf("  %s\n", d.Encoder.Reason)
		}
		return nil
	}
	var resp aigw.ClassifyResponse
	if err := c.do(http.MethodPost, "/v1/classify", req, &resp); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	esc := ""
	if resp.Escalate && resp.Source == "chispa" {
		esc = ", escalate: Chispa is unsure and the cascade is off"
	}
	fmt.Printf("%s  (source %s, p=%.3f, %.2f ms%s)\n", resp.Label, resp.Source, resp.Prob, resp.LatencyMS, esc)
	if resp.Chispa != nil {
		fmt.Printf("  chispa: %s p=%.3f threshold=%.3f -> %s\n", resp.Chispa.Label, resp.Chispa.Prob, resp.Chispa.Threshold, resp.Chispa.Decision)
	}
	if resp.VON != nil {
		fmt.Printf("  von %s: %q in %.0f ms\n", resp.VON.Model, resp.VON.Answer, resp.VON.LatencyMS)
	}
	if resp.Degraded != "" {
		fmt.Printf("  degraded: %s\n", resp.Degraded)
	}
	return nil
}

func aiCalibrate(args []string) error {
	fs := flag.NewFlagSet("ai calibrate", flag.ExitOnError)
	target := fs.Float64("target", 0, "agreement with VON to promise (default: the task's precision, the model's, or 0.95)")
	minSupport := fs.Int("min-support", 10, "confident samples a class needs before it gets a threshold")
	dry := fs.Bool("dry-run", false, "report only; never write the model")
	asJSON := fs.Bool("json", false, "JSON output")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling ai calibrate <task> [-target P] [-dry-run]")
	}
	c, err := mk()
	if err != nil {
		return err
	}
	var rep aigw.CalibrateReport
	if err := c.do(http.MethodPost, "/v1/admin/calibrate", aigw.CalibrateRequest{
		Task: fs.Arg(0), Target: *target, MinSupport: *minSupport, DryRun: *dry,
	}, &rep); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rep)
	}
	fmt.Printf("task %s (chispa %s): %d samples with a VON answer (%d escalated, %d audited)\n",
		rep.Task, rep.Model, rep.Samples, rep.Escalated, rep.Audited)
	fmt.Printf("Chispa agrees with VON on %.3f of the sample (weighted)\n", rep.Overall)
	fmt.Printf("held-out half, target agreement %.2f:\n", rep.Target)
	fmt.Printf("  before: coverage %.3f, agreement %.3f (%d confident)\n", rep.Before.Coverage, rep.Before.Agreement, rep.Before.Confident)
	fmt.Printf("  after:  coverage %.3f, agreement %.3f (%d confident)\n", rep.After.Coverage, rep.After.Agreement, rep.After.Confident)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tOLD τ\tNEW τ\tSAMPLES")
	for _, cl := range rep.Classes {
		note := ""
		if cl.Forced {
			note = "  (task override still wins)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d%s\n", cl.Label, tau(cl.Old), tau(cl.New), cl.Tuned, note)
	}
	_ = tw.Flush()
	fmt.Println(rep.Reason)
	switch {
	case rep.Written != "":
		fmt.Printf("wrote %s (previous model in %s)\n", rep.Written, rep.Backup)
	case rep.Improved:
		fmt.Println("dry run: nothing written")
	default:
		fmt.Println("nothing written")
	}
	fmt.Println("note:", rep.TeacherErr)
	if rep.Cascade != "" {
		fmt.Println(rep.Cascade)
	}
	return nil
}

func aiGenerate(args []string) error {
	fs := flag.NewFlagSet("ai generate", flag.ExitOnError)
	var vars kvFlag
	fs.Var(&vars, "var", "template variable name=value (repeatable)")
	maxTokens := fs.Int("max-tokens", 0, "at most the task's max_tokens")
	temp := fs.Float64("temperature", -1, "sampling temperature (default: the task's)")
	asJSON := fs.Bool("json", false, "print the full JSON answer")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling ai generate <task> [-var k=v] <input...>   (no input: read stdin)")
	}
	input := strings.Join(fs.Args()[1:], " ")
	if fs.NArg() == 1 {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10+1))
		if err != nil {
			return err
		}
		input = string(b)
	}
	req := aigw.GenerateRequest{Task: fs.Arg(0), Input: input, Vars: map[string]string(vars), MaxTokens: *maxTokens}
	if *temp >= 0 {
		req.Temperature = temp
	}
	c, err := mk()
	if err != nil {
		return err
	}
	var resp aigw.GenerateResponse
	if err := c.do(http.MethodPost, "/v1/generate", req, &resp); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	fmt.Println(strings.TrimSpace(resp.Output))
	fmt.Fprintf(os.Stderr, "(%s: %d+%d tokens, %.0f ms, %s)\n", resp.Model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.LatencyMS, resp.FinishReason)
	return nil
}

// kvFlag es un -var k=v repetible.
type kvFlag map[string]string

func (k *kvFlag) String() string { return fmt.Sprint(map[string]string(*k)) }

func (k *kvFlag) Set(v string) error {
	name, val, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("want name=value, got %q", v)
	}
	if *k == nil {
		*k = kvFlag{}
	}
	(*k)[name] = val
	return nil
}

// aiEval pasa un conjunto etiquetado por Chispa solo y por la cascada, y guarda
// el resultado con la tarea: es lo que decide si escalate_to se puede activar.
func aiEval(args []string) error {
	fs := flag.NewFlagSet("ai eval", flag.ExitOnError)
	data := fs.String("data", "", "labelled JSONL ({\"text\", \"fields\", \"label\"}) NOT used to train the Chispa model")
	vonModel := fs.String("von", "", "candidate von model (default: the task's escalate_to)")
	conc := fs.Int("concurrency", 1, "escalations in flight at once (more wakes more replicas)")
	alone := fs.Bool("von-alone", false, "also measure VON alone on every example (all labels, no Chispa hints)")
	dry := fs.Bool("dry-run", false, "report only; don't store the record")
	asJSON := fs.Bool("json", false, "JSON output")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *data == "" {
		return fmt.Errorf("usage: kling ai eval <task> -data test.jsonl [-von <model>] [-concurrency N] [-von-alone] [-dry-run]")
	}
	c, err := mk()
	if err != nil {
		return err
	}
	if isDomoticaTask(c, fs.Arg(0)) {
		return aiEvalDomotica(c, fs.Arg(0), *data, *dry, *asJSON)
	}
	exs, err := readEvalData(*data)
	if err != nil {
		return err
	}
	c.http.Timeout = 0 // cientos de escaladas: minutos; el servidor corta si el cliente se va
	fmt.Fprintf(os.Stderr, "evaluating %d examples on task %s...\n", len(exs), fs.Arg(0))
	var out struct {
		Record  aigw.EvalRecord   `json:"record"`
		Cascade aigw.CascadeState `json:"cascade"`
	}
	if err := c.do(http.MethodPost, "/v1/admin/eval", aigw.EvalRequest{
		Task: fs.Arg(0), VON: *vonModel, Data: filepath.Base(*data), Examples: exs,
		Concurrency: *conc, VONAlone: *alone, DryRun: *dry,
	}, &out); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	r, res := out.Record, out.Record.Results
	fmt.Printf("task %s: chispa %s, candidate von %s (%s), %d examples\n", r.Task, r.Chispa.Model, r.VON.Model, r.VON.Snapshot, res.Examples)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "Chispa alone\t%.3f\n", res.ChispaAccuracy)
	fmt.Fprintf(tw, "cascade\t%.3f\n", res.CascadeAccuracy)
	if res.VONAloneAccuracy != nil {
		fmt.Fprintf(tw, "VON alone\t%.3f\n", *res.VONAloneAccuracy)
	}
	fmt.Fprintf(tw, "Chispa confident\t%.3f of the examples, %.3f right\n", res.Coverage, res.ConfidentAccuracy)
	fmt.Fprintf(tw, "escalated (%d)\tChispa right %.3f, VON right %.3f (%d unknown, %d errors)\n",
		res.Escalated, res.ChispaAccuracyEscalated, res.VONAccuracyEscalated, res.VONUnknown, res.VONErrors)
	fmt.Fprintf(tw, "disagreements\tChispa only right %d, cascade only right %d (McNemar p=%.2g)\n", res.ChispaOnlyRight, res.VONOnlyRight, res.PValue)
	fmt.Fprintf(tw, "VON latency\tp50 %.0f ms, p95 %.0f ms (%.0f s in total)\n", res.VONLatencyP50MS, res.VONLatencyP95MS, res.DurationS)
	_ = tw.Flush()
	if res.UnseenLabels > 0 {
		fmt.Printf("warning: %d examples have a label the Chispa model doesn't know\n", res.UnseenLabels)
	}
	fmt.Println(r.Verdict)
	if r.Stored != "" {
		fmt.Printf("stored in %s\n", r.Stored)
	} else {
		fmt.Println("dry run: nothing stored")
	}
	switch out.Cascade.Status {
	case "on":
		fmt.Printf("cascade to %s: on\n", out.Cascade.To)
	case "forced", "refused":
		fmt.Println(out.Cascade.Reason)
	default:
		if r.BeatsChispa && r.Stored != "" {
			fmt.Printf("to use it, set \"escalate_to\": %q in the task and run kling ai reload\n", r.VON.Model)
		}
	}
	return nil
}

// readEvalData lee un JSONL etiquetado (el mismo formato que kling chispa train).
func readEvalData(path string) ([]aigw.EvalExample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []aigw.EvalExample
	dec := json.NewDecoder(io.LimitReader(f, 64<<20))
	for {
		var ex aigw.EvalExample
		if err := dec.Decode(&ex); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%s: example %d: %w", path, len(out)+1, err)
		}
		if ex.Label == "" {
			return nil, fmt.Errorf("%s: example %d has no label", path, len(out)+1)
		}
		out = append(out, ex)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no examples", path)
	}
	return out, nil
}

func tau(v float64) string {
	if v >= 2 {
		return "never"
	}
	return fmt.Sprintf("%.3f", v)
}

func aiReload(args []string) error {
	fs := flag.NewFlagSet("ai reload", flag.ExitOnError)
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	c, err := mk()
	if err != nil {
		return err
	}
	var out struct {
		Cascades []string `json:"cascades"`
	}
	if err := c.do(http.MethodPost, "/v1/admin/reload", nil, &out); err != nil {
		return err
	}
	fmt.Println("registry reloaded")
	for _, n := range out.Cascades {
		fmt.Println("  " + n)
	}
	return nil
}

// aiPrime rehace el dorado de cada modelo VON del registro con los prefijos
// fijos de sus tareas (system prompt y principio de la plantilla) ya evaluados
// dentro: la primera petición de una tarea en una réplica recién restaurada
// solo evalúa su propio texto. Medido en docs/von-cpu.md: con ~800 tokens de
// system prompt, de 4,7 s a 0,4 s en Qwen2.5-1.5B y de 1,7 s a 0,17 s en
// Qwen2.5-0.5B. Los prefijos quedan en la caché de prompts de llama-server
// (-cache-ram de `kling models add`); sin ella solo el último.
//
// No es automático: rehacer un dorado cuesta lo que cargar el modelo, y solo
// hace falta cuando cambian los prompts de las tareas. La etiqueta von.prefixes
// del dorado dice con qué prefijos se hizo, así que repetirlo sin cambios no
// hace nada.
func aiPrime(args []string) error {
	fs := flag.NewFlagSet("ai prime", flag.ExitOnError)
	host := hostFlag(fs)
	cfgPath := fs.String("config", aiDefault("ai.json"), "model and task registry")
	dryRun := fs.Bool("dry-run", false, "only say what would be done")
	force := fs.Bool("force", false, "remake the golden snapshot even if its prefixes are up to date")
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for the model to load")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	cfg, err := aigw.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	models := fs.Args()
	if len(models) == 0 {
		for _, n := range sortedNames(cfg.Models) {
			if cfg.Models[n].Kind == aigw.KindVON {
				models = append(models, n)
			}
		}
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return err
	}
	byName := map[string]*api.Snapshot{}
	for _, s := range snaps {
		byName[s.Name] = s
	}
	var errs []error
	for _, name := range models {
		m := cfg.Models[name]
		if m == nil || (m.Kind != aigw.KindVON && m.Kind != aigw.KindEmbed) {
			errs = append(errs, fmt.Errorf("%s: not a von model of the registry", name))
			continue
		}
		if m.Kind == aigw.KindEmbed {
			errs = append(errs, fmt.Errorf("%s: is an encoder (kind embed); it never uses the prompt cache, so priming does not apply", name))
			continue
		}
		s := byName[m.Snapshot]
		if s == nil || s.Labels[von.LabelModel] == "" {
			errs = append(errs, fmt.Errorf("%s: golden snapshot %q not found on the daemon (kling models add)", name, m.Snapshot))
			continue
		}
		pre := cfg.Prefixes(name)
		if len(pre) == 0 {
			fmt.Printf("%s: its tasks have no fixed prefix (no system prompt nor fixed template text); nothing to do\n", name)
			continue
		}
		h := von.PrefixesHash(pre)
		if s.Labels[von.LabelPrefixes] == h && !*force {
			fmt.Printf("%s: %s already has the %d prefix(es) of its tasks (%s)\n", name, s.Name, len(pre), h)
			continue
		}
		if *dryRun {
			fmt.Printf("%s: would remake %s with %d task prefix(es) (%s)\n", name, s.Name, len(pre), h)
			continue
		}
		// Rehacer pasa por reemplazar el dorado, y el daemon no borra uno con
		// réplicas vivas (seguirían mapeando su memoria): mejor decirlo antes
		// de cargar el modelo que después.
		if s.Instances > 0 {
			errs = append(errs, fmt.Errorf("%s: %s has %d machine(s) restored from it (the gateway's replicas): remove them first (kling ps; kling rm <ref>)",
				name, s.Name, s.Instances))
			continue
		}
		// Sin caché de prompts en la imagen (las de antes de v0.12, o
		// -cache-ram 0) solo sobrevive el último prefijo: se hace igual, pero
		// se dice.
		if rec, err := c.ImageRecipe(ctx, s.Image); err == nil && len(pre) > 1 {
			var sp von.Spec
			if json.Unmarshal(rec.Spec, &sp) == nil && (sp.CacheRAM == nil || *sp.CacheRAM == 0) {
				fmt.Printf("  note: image %s has no prompt cache (built before v0.12 or with -cache-ram 0): only the last of the %d prefixes stays evaluated; rebuild it to keep them all\n",
					s.Image, len(pre))
			}
		}
		extra := map[string]string{}
		for k, v := range s.Labels {
			extra[k] = v
		}
		fmt.Printf("%s: remaking %s with %d task prefix(es)...\n", name, s.Name, len(pre))
		t0 := time.Now()
		g, err := von.MakeGolden(ctx, c, von.GoldenOptions{
			Image: s.Image, Snapshot: s.Name, Ref: s.Labels[von.LabelModel],
			VCPUs: s.VCPUs, MemMiB: s.MemMiB, CPUPct: s.CPUPct, AllowExec: s.AllowExec,
			Replace: true, Wait: *wait, Prefixes: pre, Labels: extra,
			Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		fmt.Printf("✓ %s  %d prefix(es), %v tokens, %s of memory, %s\n", g.Snapshot.Name, len(pre), g.PrefixTokens,
			human(g.Snapshot.MemBytes), time.Since(t0).Round(time.Second))
	}
	return errors.Join(errs...)
}
