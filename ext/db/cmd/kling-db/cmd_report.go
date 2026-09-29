package main

// kling db report: una pregunta de `kling db ask` guardada, para repetirla cada
// cierto tiempo sobre una copia FRESCA de un golden ("cada lunes: clientes
// nuevos y facturas vencidas"). Ver docs/db-ask.md.
//
//	kling db report add <nombre> -golden G -every 1w -question "..." [-out F]
//	kling db report run <nombre> [-due] [-out F]
//	kling db report ls [-json]   ·   kling db report rm <nombre>
//
// No hay demonio: quien programa es cron o un temporizador de systemd, que
// llama a `report run` (con -due, cada hora vale para todos). Cada ejecución:
// copia nueva del golden (rpt-<nombre>), ask con -yes (la definición es el
// consentimiento), resultado a un fichero 0600 o a stdout, y la copia se borra.
//
// Las garantías de ask no cambian: al modelo solo va el esquema y la pregunta;
// la SQL pasa sqlguard y se ejecuta con el rol de solo lectura, en una
// transacción READ ONLY. Filas al proveedor solo si la definición lleva
// -explain -send-data, y eso se ve en `report ls`. La definición no guarda
// ninguna clave: la de la API sale del entorno de quien ejecuta.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/askllm"
	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

const (
	// labelReport marca la copia de una ejecución: su valor es el informe.
	labelReport = "kling.db.report"

	reportsDir    = "reports"
	reportVersion = 1
	reportMaxDef  = 64 << 10
	// reportCopyTTL congela la copia si kling-db muere a mitad (la siguiente
	// ejecución la borra): más que el peor caso de una ejecución.
	reportCopyTTL = 3 * time.Hour
)

// reportPattern es el nombre de un informe: va en rutas, en una etiqueta y,
// tras "rpt-", en el nombre de la copia.
var reportPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// everyPattern es un periodo: N minutos, horas, días o semanas.
var everyPattern = regexp.MustCompile(`^([1-9][0-9]{0,3})(m|h|d|w)$`)

func parseEvery(s string) (time.Duration, error) {
	m := everyPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid -every %q: a number and m, h, d or w (30m, 6h, 1d, 1w)", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

// reportDef es lo que se guarda (JSON, 0600, en reports/<nombre>.json).
type reportDef struct {
	Version    int    `json:"version"`
	Name       string `json:"name"`
	Golden     string `json:"golden"`
	Owner      string `json:"owner"`
	Every      string `json:"every"`
	Question   string `json:"question"`
	Role       string `json:"role,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Limit      int    `json:"limit"`
	Timeout    string `json:"timeout"`
	LLMTimeout string `json:"llm_timeout"`
	JSON       bool   `json:"json,omitempty"`
	Explain    bool   `json:"explain,omitempty"`
	SendData   bool   `json:"send_data,omitempty"`
	Out        string `json:"out,omitempty"`
	Created    string `json:"created"`
}

// opts convierte la definición en las opciones de ask, validándola entera:
// un fichero tocado a mano no se salta ninguna comprobación de ask.
func (d *reportDef) opts() (askOpts, error) {
	o := askOpts{yes: true, role: d.Role, provider: d.Provider, model: d.Model, limit: d.Limit,
		jsonOut: d.JSON, explain: d.Explain, sendData: d.SendData}
	var err error
	if o.timeout, err = time.ParseDuration(d.Timeout); err != nil {
		return o, fmt.Errorf("report %s: bad timeout %q", d.Name, d.Timeout)
	}
	if o.llmTime, err = time.ParseDuration(d.LLMTimeout); err != nil {
		return o, fmt.Errorf("report %s: bad llm_timeout %q", d.Name, d.LLMTimeout)
	}
	switch {
	case d.Version != reportVersion:
		return o, fmt.Errorf("report %s: unknown version %d", d.Name, d.Version)
	case !reportPattern.MatchString(d.Name):
		return o, fmt.Errorf("invalid report name %q", d.Name)
	case !namePattern.MatchString(d.Golden):
		return o, fmt.Errorf("report %s: invalid golden %q", d.Name, d.Golden)
	case d.Out != "" && !filepath.IsAbs(d.Out):
		return o, fmt.Errorf("report %s: the output path must be absolute", d.Name)
	}
	if err := validOwner(d.Owner); err != nil {
		return o, err
	}
	if _, err := parseEvery(d.Every); err != nil {
		return o, err
	}
	if err := o.check(d.Question); err != nil {
		return o, fmt.Errorf("report %s: %w", d.Name, err)
	}
	return o, nil
}

func reportPath(name string) (string, error) {
	if !reportPattern.MatchString(name) {
		return "", fmt.Errorf("invalid report name %q: lowercase letters, digits, '_' and '-', up to 40", name)
	}
	dir, err := dbstate.EnsureDir(reportsDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

func loadReport(name string) (*reportDef, error) {
	p, err := reportPath(name)
	if err != nil {
		return nil, err
	}
	b, err := dbstate.ReadPrivate(p, reportMaxDef)
	if dbstate.IsNotExist(err) {
		return nil, fmt.Errorf("no report %q (kling db report ls)", name)
	}
	if err != nil {
		return nil, err
	}
	var d reportDef
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("report %s: %w", name, err)
	}
	if d.Name != name {
		return nil, fmt.Errorf("report %s: the file says it is %q", name, d.Name)
	}
	if _, err := d.opts(); err != nil {
		return nil, err
	}
	return &d, nil
}

// lastRun es la hora de la última ejecución buena (cero si no hubo).
func lastRun(name string) time.Time {
	p, err := reportPath(name)
	if err != nil {
		return time.Time{}
	}
	b, err := dbstate.ReadPrivate(strings.TrimSuffix(p, ".json")+".last", 64)
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	return t
}

const reportUsage = `usage: kling db report add <name> -golden G -every 1w -question "..." [-out FILE] [-role R] [-provider P] [-model M] [-limit N] [-json] [-explain -send-data] [-replace]
       kling db report run <name> [-due] [-out FILE]
       kling db report ls [-json]
       kling db report rm <name>`

func cmdReport(args []string) error {
	fs, host, owner := newFlags("report")
	golden := fs.String("golden", "", "add: template each run starts a fresh copy from")
	every := fs.String("every", "", "add: how often it is due (30m, 6h, 1d, 1w); run -due uses it")
	question := fs.String("question", "", "add: the question, in plain words")
	out := fs.String("out", "", "add: file each run writes (mode 0600; default: stdout); run: this file instead")
	replace := fs.Bool("replace", false, "add: overwrite a report with the same name")
	due := fs.Bool("due", false, "run: do nothing unless the last good run is older than -every")
	asJSON := fs.Bool("json", false, "add: the result as JSON; ls: JSON output")
	var o askOpts
	fs.StringVar(&o.role, "role", "", "add: read-only role to run as (default: "+defaultRORole+")")
	fs.StringVar(&o.provider, "provider", "", "add: model provider: anthropic or opencode (default: as kling db ask)")
	fs.StringVar(&o.model, "model", "", "add: model")
	fs.DurationVar(&o.llmTime, "llm-timeout", askllm.DefaultLLMTimeout, "add: how long to wait for the model (opencode provider)")
	fs.IntVar(&o.limit, "limit", askDefaultLimit, fmt.Sprintf("add: maximum rows returned (1-%d)", askMaxLimit))
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "add: statement_timeout of the query")
	fs.BoolVar(&o.explain, "explain", false, "add: have the model summarize the rows (needs -send-data)")
	fs.BoolVar(&o.sendData, "send-data", false, "add: allow -explain to send up to 50 result rows to the model provider on every run")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("%s", reportUsage)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	// Qué flags vale cada subcomando: uno que no toca es un error, no se ignora.
	allowed := map[string]string{
		"add": "golden every question out replace json role provider model llm-timeout limit timeout explain send-data owner",
		"run": "due out H",
		"ls":  "json",
		"rm":  "",
	}
	sub := pos[0]
	ok, known := allowed[sub]
	if !known {
		return usageErr("%s", reportUsage)
	}
	for f := range set {
		if !strings.Contains(" "+ok+" ", " "+f+" ") {
			return usageErr("-%s does not go with report %s\n%s", f, sub, reportUsage)
		}
	}
	switch sub {
	case "ls":
		if len(pos) != 1 {
			return usageErr("%s", reportUsage)
		}
		return reportLs(os.Stdout, time.Now(), *asJSON)
	case "rm":
		if len(pos) != 2 {
			return usageErr("%s", reportUsage)
		}
		return reportRm(os.Stdout, pos[1])
	case "add":
		if len(pos) != 2 || *golden == "" || *every == "" || *question == "" {
			return usageErr("%s", reportUsage)
		}
		o.jsonOut = *asJSON
		d, err := newReportDef(pos[1], *golden, *owner, *every, *question, *out, o, time.Now())
		if err != nil {
			return err
		}
		return addReport(os.Stdout, d, *replace)
	}
	if len(pos) != 2 {
		return usageErr("%s", reportUsage)
	}
	d, err := loadReport(pos[1])
	if err != nil {
		return err
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	outPath := d.Out
	if *out != "" {
		if outPath, err = absOut(*out); err != nil {
			return err
		}
	}
	return a.reportRun(ctx, d, outPath, *due)
}

// absOut valida y hace absoluta la ruta de salida (cron no corre en el
// directorio de quien añadió el informe).
func absOut(p string) (string, error) {
	if p == "-" {
		return "", errors.New("-out needs a file; without -out the result goes to stdout")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return "", fmt.Errorf("-out %s is a directory", abs)
	}
	if st, err := os.Stat(filepath.Dir(abs)); err != nil || !st.IsDir() {
		return "", fmt.Errorf("-out %s: its directory does not exist", abs)
	}
	return abs, nil
}

func newReportDef(name, golden, owner, every, question, out string, o askOpts, now time.Time) (*reportDef, error) {
	d := &reportDef{Version: reportVersion, Name: name, Golden: golden, Owner: owner, Every: every,
		Question: strings.TrimSpace(question), Role: o.role, Provider: o.provider, Model: o.model, Limit: o.limit,
		Timeout: o.timeout.String(), LLMTimeout: o.llmTime.String(), JSON: o.jsonOut, Explain: o.explain,
		SendData: o.sendData, Created: now.UTC().Format(time.RFC3339)}
	if out != "" {
		abs, err := absOut(out)
		if err != nil {
			return nil, err
		}
		d.Out = abs
	}
	if _, err := d.opts(); err != nil {
		return nil, err
	}
	return d, nil
}

func addReport(w io.Writer, d *reportDef, replace bool) error {
	p, err := reportPath(d.Name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil && !replace {
		return fmt.Errorf("report %s already exists (-replace overwrites it)", d.Name)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := dbstate.WritePrivate(p, append(b, '\n')); err != nil {
		return err
	}
	fmt.Fprintf(w, "report %s saved in %s (mode 0600)\n", d.Name, p)
	if d.SendData {
		fmt.Fprintf(w, "  every run sends up to %d result rows to the model provider (-explain -send-data)\n", askExplainRows)
	} else {
		fmt.Fprintln(w, "  the model gets the schema and the question, never rows")
	}
	fmt.Fprintf(w, "  try it now:   kling db report run %s\n", d.Name)
	fmt.Fprintf(w, "  schedule it:  crontab -e, then:  17 * * * *  kling db report run %s -due   (see docs/db-ask.md)\n", d.Name)
	return nil
}

func reportRm(w io.Writer, name string) error {
	p, err := reportPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no report %q", name)
		}
		return err
	}
	_ = os.Remove(strings.TrimSuffix(p, ".json") + ".last")
	fmt.Fprintln(w, name)
	return nil
}

// reportRow es una fila de report ls.
type reportRow struct {
	*reportDef
	LastRun string `json:"last_run,omitempty"`
	Due     bool   `json:"due"`
	Error   string `json:"error,omitempty"`
}

func reportLs(w io.Writer, now time.Time, asJSON bool) error {
	dir, err := dbstate.EnsureDir(reportsDir)
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	rows := []reportRow{}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !reportPattern.MatchString(name) {
			continue
		}
		d, err := loadReport(name)
		if err != nil {
			rows = append(rows, reportRow{reportDef: &reportDef{Name: name}, Error: err.Error()})
			continue
		}
		r := reportRow{reportDef: d}
		every, _ := parseEvery(d.Every)
		last := lastRun(name)
		if !last.IsZero() {
			r.LastRun = last.UTC().Format(time.RFC3339)
		}
		r.Due = last.IsZero() || now.Sub(last) >= every
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, `no reports (kling db report add <name> -golden G -every 1w -question "...")`)
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "REPORT\tGOLDEN\tEVERY\tLAST RUN\tDUE\tROWS TO MODEL\tOUTPUT\tQUESTION")
	for _, r := range rows {
		if r.Error != "" {
			fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\t-\t(broken: %s)\n", r.Name, cell(r.Error))
			continue
		}
		last, due, send, out := "-", "no", "no", "stdout"
		if r.LastRun != "" {
			last = r.LastRun
		}
		if r.Due {
			due = "yes"
		}
		if r.SendData {
			send = "yes (-send-data)"
		}
		if r.Out != "" {
			out = r.Out
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Golden, r.Every, last, due, send, out, cell(r.Question))
	}
	return tw.Flush()
}

// reportCopyName es el nombre de la copia de una ejecución.
func reportCopyName(name string) string { return "rpt-" + name }

// reportRun ejecuta el informe d y escribe el resultado en outPath (vacío:
// stdout). Con due, no hace nada si la última ejecución buena es reciente.
func (a *app) reportRun(ctx context.Context, d *reportDef, outPath string, due bool) error {
	o, err := d.opts()
	if err != nil {
		return err
	}
	// Una ejecución a la vez por informe: la segunda borraría la copia de la
	// primera creyéndola abandonada.
	un, err := dbstate.Lock(ctx, "report-"+d.Name, 0, nil)
	if errors.Is(err, dbstate.ErrLocked) {
		return fmt.Errorf("report %s is already running", d.Name)
	}
	if err != nil {
		return err
	}
	defer un()
	now := a.clock()
	if due {
		every, _ := parseEvery(d.Every)
		if last := lastRun(d.Name); !last.IsZero() && now.Sub(last) < every {
			fmt.Fprintf(a.stderr, "report %s is not due until %s\n", d.Name, last.Add(every).UTC().Format(time.RFC3339))
			return nil
		}
	}
	// El proveedor antes que la copia: sin clave (o sin opencode) no se crea nada.
	prov, err := newProvider(o.provider, o.model, o.llmTime)
	if err != nil {
		return err
	}
	name := reportCopyName(d.Name)
	if err := a.removeStaleReportCopy(ctx, d, name); err != nil {
		return err
	}
	mc, err := a.upFrom(ctx, d.Golden, d.Golden, name, reportCopyTTL, d.Owner, [][2]string{{labelReport, d.Name}})
	if err != nil {
		return err
	}
	// Pase lo que pase, la copia (y su clave) se van: destroy usa su propio
	// contexto, así que también tras un Ctrl-C.
	defer a.destroy(mc.ID)
	res, err := a.askRun(ctx, mc.ID, d.Question, d.Owner, o, prov)
	if err != nil {
		return fmt.Errorf("report %s: %w", d.Name, err)
	}
	body, err := reportBody(a, d, now, res, o.jsonOut)
	if err != nil {
		return err
	}
	if outPath == "" {
		if _, err := a.stdout.Write(body); err != nil {
			return err
		}
	} else {
		if err := dbstate.WritePrivate(outPath, body); err != nil {
			return fmt.Errorf("writing %s: %w", outPath, err)
		}
		fmt.Fprintf(a.stderr, "report %s written to %s (mode 0600)\n", d.Name, outPath)
	}
	p, _ := reportPath(d.Name)
	if err := dbstate.WritePrivate(strings.TrimSuffix(p, ".json")+".last", []byte(now.UTC().Format(time.RFC3339)+"\n")); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not record the run of %s: %v\n", d.Name, err)
	}
	return nil
}

// removeStaleReportCopy borra la copia de una ejecución anterior que no
// terminó (kling-db murió). Solo si es de verdad de este informe y dueño: un
// nombre ocupado por otra cosa es un error, no se toca.
func (a *app) removeStaleReportCopy(ctx context.Context, d *reportDef, name string) error {
	all, err := a.machines(ctx)
	if err != nil {
		return err
	}
	for _, mc := range all {
		if mc == nil || mc.Name != name {
			continue
		}
		if mc.Labels[labelReport] != d.Name || mc.Labels[labelOwner] != d.Owner || mc.Labels[labelGolden] == "" {
			return fmt.Errorf("the name %s is taken by a machine that is not a copy of report %s", name, d.Name)
		}
		fmt.Fprintf(a.stderr, "removing %s, left over from an unfinished run\n", name)
		return a.remove(ctx, mc)
	}
	return nil
}

// reportMeta es la cabecera de un resultado en JSON.
type reportMeta struct {
	Report   string     `json:"report"`
	Golden   string     `json:"golden"`
	RanAt    string     `json:"ran_at"`
	Question string     `json:"question"`
	Result   *askResult `json:"result"`
}

func reportBody(a *app, d *reportDef, now time.Time, res *askResult, asJSON bool) ([]byte, error) {
	if asJSON {
		b, err := json.MarshalIndent(reportMeta{Report: d.Name, Golden: d.Golden, RanAt: now.UTC().Format(time.RFC3339),
			Question: d.Question, Result: res}, "", "  ")
		return append(b, '\n'), err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Report %s — %s (UTC), fresh copy of %s\n", d.Name, now.UTC().Format("2006-01-02 15:04"), d.Golden)
	fmt.Fprintf(&sb, "Question: %s\n\n", printable(strings.ReplaceAll(d.Question, "\n", " ")))
	ta := *a
	ta.stdout = &sb
	ta.printTable(res)
	fmt.Fprintf(&sb, "\nSQL (run read-only as %s):\n%s\n", res.Role, indent(printable(res.SQL)))
	return []byte(sb.String()), nil
}
