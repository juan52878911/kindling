package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// phoneTTL: sin TTL propio (0) el daemon pone el de su configuración y el
// teléfono se congelaría solo a los 10 min. Un año equivale a no tenerlo.
const phoneTTL = 8760 * 3600

// newResult es un teléfono recién hecho y lo que costó cada paso.
type newResult struct {
	M        *api.Machine
	Restore  time.Duration
	Identity time.Duration
	API      time.Duration
}

// newPhone restaura un clon del dorado y le da su identidad y su token: al
// volver, la API del teléfono contesta con ESE token y la serie es la suya.
func (a *app) newPhone(ctx context.Context, name, golden string, labels map[string]string) (*newResult, error) {
	if err := a.waitMemory(ctx); err != nil {
		return nil, err
	}
	snap, err := a.d.Snapshot(ctx, golden)
	if err != nil {
		if api.IsNotFound(err) {
			return nil, fmt.Errorf("%w %s (kling phone golden build)", errNoGolden, golden)
		}
		return nil, err
	}
	// run -from sin -egress pone "none" en daemons anteriores a v0.17: se le
	// pasa el del dorado.
	egress := snap.Egress
	if egress == "" {
		egress = "none"
	}
	t0 := a.now()
	m, err := a.d.Run(ctx, api.RunRequest{
		From: golden, Name: name, TTLSeconds: phoneTTL, Egress: egress,
		AllowDomains: snap.AllowDomains,
		// El dorado se guardó listo: la copia lo está al terminar sus ganchos
		// (sin identidad aún en MMDS no hacen nada).
		WaitReady: true, ReadyTimeoutSeconds: 180,
		Labels: api.MergeLabels(map[string]string{labelGolden: golden}, labels),
	})
	if err != nil {
		a.unreserve(name)
		return nil, fmt.Errorf("run -from %s: %w", golden, err)
	}
	r := &newResult{M: m, Restore: a.now().Sub(t0)}
	fail := func(err error) (*newResult, error) {
		return r, fmt.Errorf("%w (the machine is kept for inspection: kling phone rm %s)", err, name)
	}

	if err := a.giveIdentity(ctx, r); err != nil {
		return fail(err)
	}
	return r, nil
}

// giveIdentity da a un teléfono recién restaurado su identidad y su token, y
// lo comprueba por la API con ese token. Rellena r.Identity y r.API.
func (a *app) giveIdentity(ctx context.Context, r *newResult) error {
	m := r.M
	// El token antes que la identidad: si el gancho la aplica, el token con el
	// que se abre ya está guardado.
	tok, err := newToken()
	if err != nil {
		return err
	}
	rec := &tokenRec{Machine: m.ID, Name: m.Name, Control: tok, Created: a.now().UTC()}
	if err := a.putToken(ctx, rec); err != nil {
		return fmt.Errorf("saving its API token: %w", err)
	}
	doc, err := a.newIdentity(m.Name, rec)
	if err != nil {
		return err
	}
	t1 := a.now()
	if err := a.applyDoc(ctx, m, doc); err != nil {
		return err
	}
	r.Identity = a.now().Sub(t1)

	// Comprobado por la API con el token: la serie que dice el teléfono es la
	// que se le dio (y el token abre).
	t2 := a.now()
	resp, err := a.call(ctx, m, tok, "GET", "/v1/identity", nil, false)
	if err != nil {
		return err
	}
	var got struct {
		Serial    string `json:"serial"`
		AndroidID string `json:"android_id"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		return fmt.Errorf("/v1/identity: %w", err)
	}
	if got.Serial != doc.Phone.Serial || got.AndroidID != doc.Phone.AndroidID {
		return errors.New("the phone does not report the identity it was given")
	}
	if _, err := a.waitHealthy(ctx, m, 30*time.Second); err != nil {
		return err
	}
	r.API = a.now().Sub(t2)
	if m2, err := a.d.Get(ctx, m.ID); err == nil {
		r.M = m2
	}
	return nil
}

// ── adopt ────────────────────────────────────────────────────────────────────

func cmdAdopt(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	host := hostFlag(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(errors.New("usage: kling phone adopt <machine>"))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	r, err := a.adopt(ctx, pos[0])
	if err != nil {
		return err
	}
	a.printNew(r)
	return nil
}

// adopt da identidad y token a un teléfono que no hizo kling phone: un nodo de
// grafo `from: <dorado>` (sus máquinas las crea el daemon), o una copia de uno.
// Sin esto su API sigue cerrada (docs/phoned.md, #110).
func (a *app) adopt(ctx context.Context, ref string) (*newResult, error) {
	m, err := a.d.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !isPhone(m) {
		return nil, fmt.Errorf("%s is not a phone (no %s label: was it made from a golden of kling phone?)", m.Name, labelPhone)
	}
	if m.State != api.StateRunning {
		return nil, fmt.Errorf("%s is %s: it has to be running", m.Name, m.State)
	}
	if _, err := a.token(ctx, m); err == nil {
		return nil, fmt.Errorf("%s already has its identity and token (kling phone token %s -rotate for a new token)", m.Name, m.Name)
	}
	r := &newResult{M: m}
	if err := a.giveIdentity(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// ensureGolden construye el dorado si falta.
func (a *app) ensureGolden(ctx context.Context, golden string) error {
	if _, err := a.d.Snapshot(ctx, golden); err == nil {
		return nil
	} else if !api.IsNotFound(err) {
		return err
	}
	fmt.Fprintf(a.errw, "==> no golden %s: building it from image %s\n", golden, a.s.Image)
	_, err := a.goldenBuild(ctx, goldenOpts{Name: golden, Image: a.s.Image, CPUs: a.s.CPUs, MemMiB: a.s.MemMiB, Egress: a.s.Egress})
	return err
}

func (a *app) printNew(r *newResult) {
	addr := adbAddr(r.M)
	if addr == "" {
		addr = "-"
	}
	fmt.Fprintf(a.out, "%s\tadb %s\t(restore %s, identity %s, api %s)\n", r.M.Name, addr,
		secs(r.Restore), secs(r.Identity), secs(r.API))
}

// ── up ───────────────────────────────────────────────────────────────────────

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	host := hostFlag(fs)
	n := fs.Int("n", 1, "how many phones")
	golden := fs.String("golden", "", "golden template (default: phone.golden, or phone-golden)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || *n < 1 {
		return usageErr(errors.New("usage: kling phone up [-n N] [-golden G]"))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	return a.up(ctx, *n, or(*golden, a.s.Golden))
}

func (a *app) up(ctx context.Context, n int, golden string) error {
	if err := a.ensureGolden(ctx, golden); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		name, err := a.nextName(ctx)
		if err != nil {
			return err
		}
		r, err := a.newPhone(ctx, name, golden, nil)
		if err != nil {
			return err
		}
		a.printNew(r)
	}
	return nil
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// ── ls ───────────────────────────────────────────────────────────────────────

type lsRow struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	State   string `json:"state"`
	ADB     string `json:"adb,omitempty"`
	API     string `json:"api"`
	Golden  string `json:"golden,omitempty"`
	Pool    string `json:"pool,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Created string `json:"created"`
}

func cmdLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	host := hostFlag(fs)
	js := fs.Bool("json", false, "JSON output")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	return newApp(*host).ls(ctx, *js)
}

func (a *app) ls(ctx context.Context, js bool) error {
	ps, err := a.phones(ctx)
	if err != nil {
		return err
	}
	rows := make([]lsRow, len(ps))
	var wg sync.WaitGroup
	for i, m := range ps {
		rows[i] = lsRow{Name: m.Name, ID: m.ID, State: string(m.State), ADB: adbAddr(m), API: "-",
			Golden: m.Labels[labelGolden], Pool: m.Labels[labelPool], Owner: m.Labels[labelOwner],
			Created: m.CreatedAt.UTC().Format(time.RFC3339)}
		if m.State != api.StateRunning {
			continue
		}
		wg.Add(1)
		go func(i int, m *api.Machine) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			h, err := a.health(c, m)
			switch {
			case err != nil && h == nil:
				rows[i].API = "error"
			case h.APITokens == 0:
				rows[i].API = "locked"
			case h.OK:
				rows[i].API = "ok"
			default:
				rows[i].API = "unhealthy"
			}
		}(i, m)
	}
	wg.Wait()
	if js {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tADB\tAPI\tGOLDEN\tPOOL")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.State, dash(r.ADB), r.API, dash(r.Golden), dash(r.Pool))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ── rm, pause, resume ────────────────────────────────────────────────────────

func cmdRm(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	host := hostFlag(fs)
	all := fs.Bool("a", false, "all phones")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 && !*all {
		return usageErr(errors.New("usage: kling phone rm <phone>... | -a"))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	var ms []*api.Machine
	if *all {
		if ms, err = a.phones(ctx); err != nil {
			return err
		}
	}
	for _, p := range pos {
		m, err := a.phone(ctx, p)
		if err != nil {
			return err
		}
		ms = append(ms, m)
	}
	for _, m := range ms {
		if err := a.rm(ctx, m); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s removed\n", m.Name)
	}
	return nil
}

// rm borra la máquina y su token.
func (a *app) rm(ctx context.Context, m *api.Machine) error {
	if err := a.d.Remove(ctx, m.ID); err != nil && !api.IsNotFound(err) {
		return err
	}
	if err := a.delToken(ctx, m.ID); err != nil {
		return fmt.Errorf("%s: removing its API token: %w", m.Name, err)
	}
	return nil
}

func cmdPause(args []string) error {
	return eachPhone("pause", args, func(ctx context.Context, a *app, m *api.Machine) error {
		if _, err := a.d.Pause(ctx, m.ID); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s paused\n", m.Name)
		return nil
	})
}

func cmdResume(args []string) error {
	return eachPhone("resume", args, func(ctx context.Context, a *app, m *api.Machine) error {
		t0 := a.now()
		m2, err := a.resume(ctx, m)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s\tadb %s\t(resume %s)\n", m2.Name, dash(adbAddr(m2)), secs(a.now().Sub(t0)))
		return nil
	})
}

// resume descongela (o reanuda) y espera a su API.
func (a *app) resume(ctx context.Context, m *api.Machine) (*api.Machine, error) {
	if m.State != api.StateRunning {
		var err error
		if m, err = a.d.Thaw(ctx, m.ID); err != nil {
			return nil, err
		}
	}
	if _, err := a.waitHealthy(ctx, m, 60*time.Second); err != nil {
		return nil, err
	}
	return m, nil
}

func eachPhone(cmd string, args []string, fn func(context.Context, *app, *api.Machine) error) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	host := hostFlag(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr(fmt.Errorf("usage: kling phone %s <phone>...", cmd))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	for _, p := range pos {
		m, err := a.phone(ctx, p)
		if err != nil {
			return err
		}
		if err := fn(ctx, a, m); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	return nil
}

// ── adb ──────────────────────────────────────────────────────────────────────

func cmdAdb(args []string) error {
	if len(args) < 1 {
		return usageErr(errors.New("usage: kling phone adb <phone> [adb args...]"))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp("")
	m, err := a.phone(ctx, args[0])
	if err != nil {
		return err
	}
	addr := adbAddr(m)
	if addr == "" {
		return fmt.Errorf("%s has no adb address (state %s; paused? kling phone resume %s)", m.Name, m.State, m.Name)
	}
	if len(args) == 1 {
		fmt.Fprintln(a.out, addr)
		return nil
	}
	adb, err := exec.LookPath("adb")
	if err != nil {
		return errors.New("adb is not installed (brew install android-platform-tools)")
	}
	_ = exec.Command(adb, "connect", addr).Run()
	c := exec.CommandContext(ctx, adb, append([]string{"-s", addr}, args[1:]...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// ── api ──────────────────────────────────────────────────────────────────────

func cmdAPI(args []string) error {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	host := hostFlag(fs)
	noTok := fs.Bool("no-token", false, "send no token (to see what an edge without one gets)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 3 || len(pos) > 4 {
		return usageErr(errors.New("usage: kling phone api <phone> <METHOD> <path> [body-file|-]   (e.g. api 1 GET /v1/health)"))
	}
	var body []byte
	if len(pos) == 4 {
		if pos[3] == "-" {
			body, err = io.ReadAll(io.LimitReader(os.Stdin, 200<<20))
		} else {
			body, err = os.ReadFile(pos[3])
		}
		if err != nil {
			return err
		}
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	m, err := a.phone(ctx, pos[0])
	if err != nil {
		return err
	}
	method := strings.ToUpper(pos[1])
	// El APK va como binario: call lo pasa a base64 para el proxy.
	binary := method == "POST" && strings.HasPrefix(pos[2], "/v1/install") && !strings.Contains(pos[2], "encoding=")
	var r *phoneResp
	if *noTok {
		r, err = a.call(ctx, m, "", method, pos[2], body, binary)
	} else {
		r, err = a.callPhone(ctx, m, method, pos[2], body, binary)
	}
	if r != nil {
		_, _ = a.out.Write(r.Body)
		if len(r.Body) > 0 && r.Body[len(r.Body)-1] != '\n' && !strings.HasPrefix(r.ContentType, "image/") {
			fmt.Fprintln(a.out)
		}
	}
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			return &plugin.ExitError{Code: 2, Err: err}
		}
		return err
	}
	return nil
}

// ── token ────────────────────────────────────────────────────────────────────

func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	host := hostFlag(fs)
	read := fs.Bool("read", false, "mint an extra read-only token (screen, tree) and print it")
	rotate := fs.Bool("rotate", false, "replace every token of the phone with a new control token")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*read && *rotate) {
		return usageErr(errors.New("usage: kling phone token <phone> [-read | -rotate]"))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	m, err := a.phone(ctx, pos[0])
	if err != nil {
		return err
	}
	tok, err := a.tokenCmd(ctx, m, *read, *rotate)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, tok)
	return nil
}

// tokenCmd imprime el token de control, o acuña uno de lectura, o los rota.
// Rotar y acuñar mandan al teléfono un documento solo de tokens (no rehace
// su identidad).
func (a *app) tokenCmd(ctx context.Context, m *api.Machine, read, rotate bool) (string, error) {
	rec, err := a.token(ctx, m)
	if err != nil {
		return "", err
	}
	if !read && !rotate {
		return rec.Control, nil
	}
	if m.State != api.StateRunning {
		return "", fmt.Errorf("%s is %s: resume it first (the tokens live in its RAM)", m.Name, m.State)
	}
	next := *rec
	var out string
	if rotate {
		t, err := newToken()
		if err != nil {
			return "", err
		}
		next.Control, next.Read, out = t, nil, t
	} else {
		t, err := newToken()
		if err != nil {
			return "", err
		}
		next.Read, out = append(append([]string(nil), rec.Read...), t), t
	}
	toks := next.hashes()
	if err := a.applyDoc(ctx, m, identityDoc{Phone: phoneDoc{APITokens: &toks}}); err != nil {
		return "", err
	}
	if err := a.putToken(ctx, &next); err != nil {
		return "", err
	}
	return out, nil
}

// ── pool ─────────────────────────────────────────────────────────────────────

func cmdPool(args []string) error {
	fs := flag.NewFlagSet("pool", flag.ContinueOnError)
	host := hostFlag(fs)
	golden := fs.String("golden", "", "golden template (default: phone.golden)")
	watch := fs.Bool("watch", false, "keep refilling, and freeze spares paused for longer than -freeze-after")
	freezeAfter := fs.Duration("freeze-after", 30*time.Minute, "with -watch: a paused spare this old is frozen (RAM freed; resume ~1 s)")
	every := fs.Duration("interval", 30*time.Second, "with -watch: how often to look")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(errors.New("usage: kling phone pool <N> [-watch] [-freeze-after 30m]"))
	}
	n, err := strconv.Atoi(pos[0])
	if err != nil || n < 0 {
		return usageErr(errors.New("pool: N must be a number >= 0"))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	g := or(*golden, a.s.Golden)
	if err := a.ensureGolden(ctx, g); err != nil {
		return err
	}
	for {
		if err := a.poolFill(ctx, g, n); err != nil {
			return err
		}
		if !*watch {
			return nil
		}
		if err := a.poolFreeze(ctx, *freezeAfter); err != nil {
			fmt.Fprintf(a.errw, "pool: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
	}
}

// spares son los repuestos del fondo de ese dorado.
func (a *app) spares(ctx context.Context, golden string) ([]*api.Machine, error) {
	ps, err := a.phones(ctx)
	if err != nil {
		return nil, err
	}
	var out []*api.Machine
	for _, m := range ps {
		if m.Labels[labelPool] == poolSpare && m.Labels[labelGolden] == golden {
			out = append(out, m)
		}
	}
	return out, nil
}

// poolFill deja n repuestos pausados: con su identidad y su token, listos
// para un resume de milisegundos.
func (a *app) poolFill(ctx context.Context, golden string, n int) error {
	have, err := a.spares(ctx, golden)
	if err != nil {
		return err
	}
	for i := len(have); i < n; i++ {
		name, err := a.nextName(ctx)
		if err != nil {
			return err
		}
		// "warming" hasta que está pausado: nadie lo reclama a medio hacer.
		r, err := a.newPhone(ctx, name, golden, map[string]string{labelPool: poolWarming})
		if err != nil {
			return err
		}
		if _, err := a.d.Pause(ctx, r.M.ID); err != nil {
			return err
		}
		if err := a.d.SetLabels(ctx, r.M.ID, map[string]string{labelPool: poolSpare,
			labelPooled: strconv.FormatInt(a.now().Unix(), 10)}); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s ready (paused spare; restore %s, identity %s)\n", r.M.Name, secs(r.Restore), secs(r.Identity))
	}
	fmt.Fprintf(a.out, "pool: %d spare phone(s) of %s\n", max(len(have), n), golden)
	return nil
}

// poolFreeze congela los repuestos que llevan pausados más de after: pausado
// no gasta CPU pero sí toda su RAM; congelado, nada (y vuelve en ~1 s).
func (a *app) poolFreeze(ctx context.Context, after time.Duration) error {
	ps, err := a.phones(ctx)
	if err != nil {
		return err
	}
	for _, m := range ps {
		if m.Labels[labelPool] != poolSpare || m.State != api.StatePaused {
			continue
		}
		t, err := strconv.ParseInt(m.Labels[labelPooled], 10, 64)
		if err != nil || a.now().Sub(time.Unix(t, 0)) < after {
			continue
		}
		if _, err := a.d.Freeze(ctx, m.ID); err != nil {
			return fmt.Errorf("%s: freeze: %w", m.Name, err)
		}
		fmt.Fprintf(a.out, "%s frozen (spare idle for more than %s)\n", m.Name, after)
	}
	return nil
}

// claim da un teléfono a un dueño: un repuesto del fondo si lo hay (resume de
// ms), si no uno nuevo. Nunca uno que ya tenga dueño.
func (a *app) claim(ctx context.Context, golden, owner string, mu *sync.Mutex) (*api.Machine, string, error) {
	mu.Lock()
	sp, err := a.spares(ctx, golden)
	var pick *api.Machine
	if err == nil && len(sp) > 0 {
		pick = sp[0]
		err = a.d.SetLabels(ctx, pick.ID, map[string]string{labelPool: poolClaimed, labelOwner: owner})
	}
	mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	if pick != nil {
		m, err := a.resume(ctx, pick)
		if err != nil {
			return nil, "", err
		}
		return m, "spare", nil
	}
	name, err := a.nextName(ctx)
	if err != nil {
		return nil, "", err
	}
	r, err := a.newPhone(ctx, name, golden, map[string]string{labelPool: poolClaimed, labelOwner: owner})
	if err != nil {
		return nil, "", err
	}
	return r.M, "new", nil
}
