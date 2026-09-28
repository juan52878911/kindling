// kling-mcpbench mide cuánto tarda el gateway MCP en atender una ráfaga de
// sesiones que llegan a servicios congelados (o calientes), y qué le cuesta al
// host: microVMs vivas, PSS, memoria disponible y PSI mientras dura.
//
// Es el generador de carga de docs/thaw-at-scale.md; quien monta la matriz de
// celdas, las imágenes y el gateway es ext/mcp/scripts/95-thaw-scale.sh. Solo
// biblioteca estándar: se compila en el Mac y se copia al host del lab, que no
// tiene Go.
//
//	KLING_GATEWAY_TOKEN=... kling-mcpbench -gateway http://127.0.0.1:18180 \
//	    -services tb-0..9 -sessions 50 -calls 5 -label frozen-n10-m50 -json out.json
//
// El token NUNCA por flag (la línea de comandos se lee en /proc): variable de
// entorno o -token-file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxTimeout es el ReadTimeout del servidor del gateway (cmd/kling-mcp/gateway.go):
// un plazo por petición mayor no mide nada, el gateway corta antes.
const maxTimeout = 120 * time.Second

// degradedRate: por encima de este porcentaje de sesiones fallidas la celda se
// marca DEGRADED. Se informa igual; la marca es para que nadie cite su p50
// como si fuera el de una celda sana.
const degradedRate = 1.0

type options struct {
	gateway     string
	services    []string
	sessions    int
	calls       int
	timeout     time.Duration
	sock        string
	sample      time.Duration
	settle      time.Duration
	label       string
	jsonOut     string
	md          bool
	maxSessions int
	tool        string
	args        json.RawMessage
	tokenFile   string
	psiLimit    string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "kling-mcpbench:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string, stdout io.Writer) (*options, bool, error) {
	fs := flag.NewFlagSet("kling-mcpbench", flag.ContinueOnError)
	o := &options{}
	fs.StringVar(&o.gateway, "gateway", "http://127.0.0.1:18180", "MCP gateway base URL")
	svcs := fs.String("services", "", "services to hit: a,b,c and/or ranges like tb-0..49 (required)")
	fs.IntVar(&o.sessions, "sessions", 1, "concurrent sessions, round-robin over the services")
	fs.IntVar(&o.calls, "calls", 5, "steady tools/call per session after the first one")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "per-request timeout (max 120s, the gateway's ReadTimeout)")
	fs.StringVar(&o.sock, "sock", "/run/kling.sock", "daemon socket for /metrics and /info (empty = no host sampling)")
	fs.DurationVar(&o.sample, "sample", 250*time.Millisecond, "host sampling interval")
	fs.DurationVar(&o.settle, "settle", 0, "after the round, wait up to this long for the services' microVMs to reach zero (time-to-zero)")
	fs.StringVar(&o.label, "label", "", "cell label stored in the report")
	fs.StringVar(&o.jsonOut, "json", "", "write the JSON report to this file (- = stdout)")
	fs.BoolVar(&o.md, "md", false, "print one Markdown table row to stdout")
	mdHeader := fs.Bool("md-header", false, "print the Markdown table header and exit")
	fs.IntVar(&o.maxSessions, "max-sessions", 200, "refuse to run more sessions than this (safety)")
	fs.StringVar(&o.tool, "tool", "echo", "tool to call")
	args0 := fs.String("args", `{"text":"kindling"}`, "tool arguments (JSON object)")
	fs.StringVar(&o.tokenFile, "token-file", "", "read the gateway token from this file (TOKEN or KEY=TOKEN); default: $KLING_GATEWAY_TOKEN")
	fs.StringVar(&o.psiLimit, "psi-limit", "", "the daemon's KLING_MAX_MEM_PRESSURE, recorded in the report (default: 20, the daemon default)")
	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}
	if *mdHeader {
		fmt.Fprint(stdout, mdHeaderText())
		return nil, true, nil
	}
	if fs.NArg() > 0 {
		return nil, false, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	var err error
	if o.services, err = parseServices(*svcs); err != nil {
		return nil, false, err
	}
	if o.sessions < 1 {
		return nil, false, fmt.Errorf("-sessions must be >= 1")
	}
	if o.sessions > o.maxSessions {
		return nil, false, fmt.Errorf("-sessions %d is above -max-sessions %d; raise -max-sessions if you mean it", o.sessions, o.maxSessions)
	}
	if o.calls < 0 {
		return nil, false, fmt.Errorf("-calls must be >= 0")
	}
	if o.timeout <= 0 || o.timeout > maxTimeout {
		return nil, false, fmt.Errorf("-timeout must be in (0, %s]: the gateway cuts requests at %s anyway", maxTimeout, maxTimeout)
	}
	if o.sample <= 0 {
		return nil, false, fmt.Errorf("-sample must be > 0")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(*args0), &obj); err != nil {
		return nil, false, fmt.Errorf("-args is not a JSON object: %w", err)
	}
	o.args = json.RawMessage(*args0)
	o.gateway = strings.TrimRight(o.gateway, "/")
	if o.psiLimit == "" {
		o.psiLimit = "20 (daemon default, assumed)"
	}
	return o, false, nil
}

// rangoServicio es `prefijo-A..B`: tb-0..49 son tb-0, tb-1, ..., tb-49.
var rangoServicio = regexp.MustCompile(`^(.*?)(\d+)\.\.(\d+)$`)

// maxServices acota lo que puede expandir un rango: tb-0..999999 es una errata,
// no una prueba.
const maxServices = 1000

// parseServices expande la lista de -services. Sin duplicados: dos veces el
// mismo servicio cambiaría el reparto sin que se note en la etiqueta.
func parseServices(spec string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(s string) error {
		if seen[s] {
			return fmt.Errorf("service %q listed twice in -services", s)
		}
		seen[s] = true
		out = append(out, s)
		if len(out) > maxServices {
			return fmt.Errorf("-services expands to more than %d services", maxServices)
		}
		return nil
	}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if m := rangoServicio.FindStringSubmatch(item); m != nil {
			a, _ := strconv.Atoi(m[2])
			b, _ := strconv.Atoi(m[3])
			if b < a || b-a >= maxServices {
				return nil, fmt.Errorf("bad range %q", item)
			}
			for i := a; i <= b; i++ {
				if err := add(m[1] + strconv.Itoa(i)); err != nil {
					return nil, err
				}
			}
			continue
		}
		if strings.ContainsAny(item, "/?#% ") {
			return nil, fmt.Errorf("bad service name %q", item)
		}
		if err := add(item); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, errors.New("-services is required (e.g. tb-0..9 or a,b,c)")
	}
	return out, nil
}

// readToken lee el token sin pasar nunca por argv.
func readToken(file string) (string, error) {
	if file == "" {
		return os.Getenv("KLING_GATEWAY_TOKEN"), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	// Formato de /etc/kling/gateway.env: KLING_GATEWAY_TOKEN=xxx.
	if _, v, ok := strings.Cut(t, "="); ok {
		t = strings.TrimSpace(v)
	}
	return t, nil
}

// Report es el resultado de una celda.
type Report struct {
	Label   string    `json:"label"`
	Started time.Time `json:"started"`
	Host    HostInfo  `json:"host"`
	Config  struct {
		Gateway   string   `json:"gateway"`
		Services  []string `json:"services"`
		NServices int      `json:"n_services"`
		Sessions  int      `json:"sessions"`
		Calls     int      `json:"calls"`
		Timeout   string   `json:"timeout"`
		Tool      string   `json:"tool"`
	} `json:"config"`
	SessionsOK     int     `json:"sessions_ok"`
	SessionsFailed int     `json:"sessions_failed"`
	ErrorRatePct   float64 `json:"error_rate_pct"`
	// Status es OK o DEGRADED (más del 1 % de sesiones fallidas).
	Status string `json:"status"`
	// Errors cuenta los fallos por fase:código (init:http_503, call:timeout...).
	Errors       map[string]int `json:"errors"`
	ErrorSamples []string       `json:"error_samples,omitempty"`
	WallMS       float64        `json:"wall_ms"`
	Latency      struct {
		// TTFR: initialize → primer resultado de herramienta. La cifra titular.
		TTFR      Dist `json:"ttfr"`
		Init      Dist `json:"init"`
		FirstCall Dist `json:"first_call"`
		Steady    Dist `json:"steady"`
		Delete    Dist `json:"delete"`
	} `json:"latency_ms"`
	// Metrics es nil si no se pudo muestrear el daemon (-sock vacío o sin él).
	Metrics *HostMetrics `json:"host_metrics"`
}

func buildReport(o *options, started time.Time, host HostInfo, results []sessionResult, wall time.Duration) *Report {
	r := &Report{Label: o.label, Started: started.UTC(), Host: host, Errors: map[string]int{}}
	r.Config.Gateway = o.gateway
	r.Config.Services = o.services
	r.Config.NServices = len(o.services)
	r.Config.Sessions = o.sessions
	r.Config.Calls = o.calls
	r.Config.Timeout = o.timeout.String()
	r.Config.Tool = o.tool

	var ttfr, ini, first, steady, del []float64
	for _, s := range results {
		if s.Err != nil {
			r.SessionsFailed++
			r.Errors[s.Err.Stage+":"+s.Err.Code]++
			if len(r.ErrorSamples) < 5 {
				r.ErrorSamples = append(r.ErrorSamples, s.Service+": "+s.Err.Error())
			}
		} else {
			r.SessionsOK++
			ttfr = append(ttfr, s.TTFRMS)
			first = append(first, s.FirstCallMS)
		}
		// init y delete cuentan aunque luego fallara otra fase: ocurrieron.
		if s.InitMS > 0 {
			ini = append(ini, s.InitMS)
		}
		if s.DeleteMS > 0 {
			del = append(del, s.DeleteMS)
		}
		steady = append(steady, s.Steady...)
	}
	r.Latency.TTFR = summarize(ttfr)
	r.Latency.Init = summarize(ini)
	r.Latency.FirstCall = summarize(first)
	r.Latency.Steady = summarize(steady)
	r.Latency.Delete = summarize(del)
	r.ErrorRatePct = round2(100 * float64(r.SessionsFailed) / float64(len(results)))
	r.Status = "OK"
	if r.ErrorRatePct > degradedRate {
		r.Status = "DEGRADED"
	}
	r.WallMS = round2(ms(wall))
	return r
}

func mdHeaderText() string {
	return "| cell | N | M | ok | TTFR p50 | p95 | p99 | max | steady p50 | p99 | live peak | machines | PSS peak MiB | min avail MiB | PSI max | t→0 s | status |\n" +
		"|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n"
}

func (r *Report) mdRow() string {
	na := "n/a"
	live, machines, pss, avail, psi, t0 := na, na, na, na, na, na
	if m := r.Metrics; m != nil {
		live = strconv.Itoa(m.PeakLive)
		machines = strconv.Itoa(m.DistinctMachines)
		pss = fmt.Sprintf("%.0f", m.PeakTargetPSSMiB)
		avail = fmt.Sprintf("%.0f", m.MinAvailableMiB)
		if m.MaxPSISomeAvg10 >= 0 {
			psi = fmt.Sprintf("%.1f", m.MaxPSISomeAvg10)
		}
		if m.TimeToZeroMS != nil {
			t0 = fmt.Sprintf("%.1f", *m.TimeToZeroMS/1000)
		}
	}
	l := r.Latency
	return fmt.Sprintf("| %s | %d | %d | %d/%d | %.0f | %.0f | %.0f | %.0f | %.1f | %.1f | %s | %s | %s | %s | %s | %s | %s |\n",
		r.Label, r.Config.NServices, r.Config.Sessions, r.SessionsOK, r.SessionsOK+r.SessionsFailed,
		l.TTFR.P50, l.TTFR.P95, l.TTFR.P99, l.TTFR.Max, l.Steady.P50, l.Steady.P99,
		live, machines, pss, avail, psi, t0, r.Status)
}

func (r *Report) human(w io.Writer) {
	fmt.Fprintf(w, "%s: %d sessions over %d service(s), %d/%d ok (%.2f%% failed) — %s, wall %.0f ms\n",
		orDash(r.Label), r.Config.Sessions, r.Config.NServices, r.SessionsOK, r.SessionsOK+r.SessionsFailed,
		r.ErrorRatePct, r.Status, r.WallMS)
	row := func(name string, d Dist) {
		if d.N == 0 {
			return
		}
		fmt.Fprintf(w, "  %-11s n=%-5d p50 %8.1f  p95 %8.1f  p99 %8.1f  max %8.1f ms\n", name, d.N, d.P50, d.P95, d.P99, d.Max)
	}
	row("ttfr", r.Latency.TTFR)
	row("initialize", r.Latency.Init)
	row("first call", r.Latency.FirstCall)
	row("steady", r.Latency.Steady)
	row("delete", r.Latency.Delete)
	if len(r.Errors) > 0 {
		keys := make([]string, 0, len(r.Errors))
		for k := range r.Errors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "  errors:")
		for _, k := range keys {
			fmt.Fprintf(w, " %s=%d", k, r.Errors[k])
		}
		fmt.Fprintln(w)
		for _, s := range r.ErrorSamples {
			fmt.Fprintf(w, "    %s\n", s)
		}
	}
	if m := r.Metrics; m != nil {
		fmt.Fprintf(w, "  host: live peak %d, %d distinct machine(s), PSS peak %.0f MiB (all: %.0f), min available %.0f MiB, PSI some avg10 max %.1f",
			m.PeakLive, m.DistinctMachines, m.PeakTargetPSSMiB, m.PeakTotalPSSMiB, m.MinAvailableMiB, m.MaxPSISomeAvg10)
		if m.TimeToZeroMS != nil {
			fmt.Fprintf(w, ", time to zero %.1f s", *m.TimeToZeroMS/1000)
		}
		fmt.Fprintf(w, " (%d samples", m.Samples)
		if m.SampleErrors > 0 {
			fmt.Fprintf(w, ", %d failed: %s", m.SampleErrors, m.LastError)
		}
		fmt.Fprintln(w, ")")
	} else {
		fmt.Fprintln(w, "  host: not sampled (no daemon socket)")
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func run(args []string, stdout, stderr io.Writer) error {
	o, done, err := parseFlags(args, stdout)
	if err != nil || done {
		return err
	}
	token, err := readToken(o.tokenFile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var dc *http.Client
	if o.sock != "" {
		if _, err := os.Stat(o.sock); err == nil {
			dc = daemonClient(o.sock)
		} else {
			fmt.Fprintf(stderr, "warning: daemon socket %s not usable (%v); host metrics will be missing\n", o.sock, err)
		}
	}
	host := hostInfo(ctx, dc, o.psiLimit)

	client := &mcpClient{
		gateway: o.gateway,
		token:   token,
		timeout: o.timeout,
		http: &http.Client{Transport: &http.Transport{
			// Una conexión por sesión como mucho: el gateway es uno, y sin
			// esto el Transport se quedaría con 2 ociosas y abriría y cerraría
			// cientos de conexiones durante la ráfaga, que es otra prueba.
			MaxIdleConns:        o.sessions + 8,
			MaxIdleConnsPerHost: o.sessions + 8,
			IdleConnTimeout:     90 * time.Second,
		}},
	}

	// Comprobación previa: /healthz, para que un gateway caído sea un error de
	// preparación y no 200 sesiones fallidas en la tabla.
	hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
	hreq, _ := http.NewRequestWithContext(hctx, http.MethodGet, o.gateway+"/healthz", nil)
	hresp, err := client.http.Do(hreq)
	hcancel()
	if err != nil {
		return fmt.Errorf("gateway %s not reachable: %w", o.gateway, err)
	}
	hresp.Body.Close()

	started := time.Now()
	var smp *sampler
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	if dc != nil {
		smp = &sampler{client: dc, targets: map[string]bool{}, psiPath: "/proc/pressure/memory", t0: started}
		for _, s := range o.services {
			smp.targets[s] = true
		}
		go smp.run(sctx, o.sample)
	}

	results := runLoad(ctx, client, o.services, o.sessions, callSpec{Tool: o.tool, Args: o.args}, o.calls)
	end := time.Since(started)
	if smp != nil {
		smp.cierre(ctx, end)
	}

	// Tiempo hasta cero: se sigue muestreando hasta que no quede ninguna
	// microVM viva de los servicios medidos, o hasta -settle.
	if smp != nil && o.settle > 0 {
		deadline := time.Now().Add(o.settle)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			if s, ok := smp.last(end); ok && s.Live == 0 {
				break
			}
			time.Sleep(o.sample)
		}
	}
	scancel()

	r := buildReport(o, started, host, results, end)
	if smp != nil {
		smp.mu.Lock()
		hm := summarizeSamples(smp.samples, end)
		hm.SampleErrors, hm.LastError = smp.errs, smp.lastErr
		smp.mu.Unlock()
		r.Metrics = &hm
	}

	r.human(stderr)
	if o.jsonOut != "" {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if o.jsonOut == "-" {
			_, err = stdout.Write(b)
		} else {
			err = os.WriteFile(o.jsonOut, b, 0o644)
		}
		if err != nil {
			return err
		}
	}
	if o.md {
		fmt.Fprint(stdout, r.mdRow())
	}
	if ctx.Err() != nil {
		return errors.New("interrupted: the report above is partial")
	}
	return nil
}
