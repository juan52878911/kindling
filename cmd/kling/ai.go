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
		return fmt.Errorf("usage: kling ai <serve|ls|test|calibrate|reload> [...]")
	}
	switch args[0] {
	case "serve":
		return aiServe(args[1:])
	case "ls", "list":
		return aiList(args[1:])
	case "test":
		return aiTest(args[1:])
	case "calibrate":
		return aiCalibrate(args[1:])
	case "reload":
		return aiReload(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q: use serve, ls, test, calibrate or reload", args[0])
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
	idle := fs.Duration("idle", 2*time.Minute, "time without requests before a replica is frozen")
	maxReplicas := fs.Int("max-replicas", 2, "replicas per model (max_replicas in the registry wins)")
	maxInflight := fs.Int("max-inflight", 1, "requests per replica before asking for another")
	keepwarm := fs.Int("keepwarm", 0, "N most used models kept awake (0 = pure scale to zero)")
	jevMem := fs.Int("jev-mem", 256, "MiB of JEV models kept loaded (LRU)")
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
		Client: client, ConfigPath: *cfgPath, Config: cfg, ID: *id,
		Idle: *idle, MaxReplicas: *maxReplicas, MaxInflight: *maxInflight, KeepWarm: *keepwarm,
		JEVBudget: int64(*jevMem) << 20, VONTimeout: *vonTimeout,
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
			if err := g.Reload(); err != nil {
				log.Printf("reload: %v", err)
			} else {
				log.Printf("registry reloaded")
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
		if m.Kind == aigw.KindVON {
			src = m.Snapshot
			reps = "default"
			if m.MaxReplicas > 0 {
				reps = fmt.Sprint(m.MaxReplicas)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, m.Kind, src, reps)
	}
	fmt.Fprintln(tw, "\nTASK\tJEV\tVON\tAUDIT\tSAMPLES")
	for _, n := range sortedNames(cfg.Tasks) {
		t := cfg.Tasks[n]
		samples := "-"
		if lt, ok := byName[n]; ok {
			samples = fmt.Sprint(lt.Samples)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%g\t%s\n", n, orDash(t.JEV), orDash(t.VON), t.Audit, samples)
	}
	if err := tw.Flush(); err != nil {
		return err
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
	mode := fs.String("mode", "", "cascade (default), jev or von")
	fields := fs.String("fields", "", `structured fields as JSON, e.g. {"service":"api"}`)
	asJSON := fs.Bool("json", false, "print the full JSON answer")
	explain := fs.Bool("explain", false, "include JEV's evidence also when it answers")
	mk := aiClientFlags(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling ai test <task> [-mode M] [-fields JSON] <text...>")
	}
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
	var resp aigw.ClassifyResponse
	if err := c.do(http.MethodPost, "/v1/classify", req, &resp); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	fmt.Printf("%s  (source %s, p=%.3f, %.2f ms)\n", resp.Label, resp.Source, resp.Prob, resp.LatencyMS)
	if resp.JEV != nil {
		fmt.Printf("  jev: %s p=%.3f threshold=%.3f -> %s\n", resp.JEV.Label, resp.JEV.Prob, resp.JEV.Threshold, resp.JEV.Decision)
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
	fmt.Printf("task %s (jev %s): %d samples with a VON answer (%d escalated, %d audited)\n",
		rep.Task, rep.Model, rep.Samples, rep.Escalated, rep.Audited)
	fmt.Printf("JEV agrees with VON on %.3f of the sample (weighted)\n", rep.Overall)
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
	return nil
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
	if err := c.do(http.MethodPost, "/v1/admin/reload", nil, nil); err != nil {
		return err
	}
	fmt.Println("registry reloaded")
	return nil
}
