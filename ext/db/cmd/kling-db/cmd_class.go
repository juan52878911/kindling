package main

// kling db class: una copia por alumno para un taller o una clase.
//
//	kling db class -n 30 [-prefix alumno] <golden>   crea alumno-01 ... alumno-30
//	kling db class ls [-prefix alumno] [-json]         las lista con cómo conectar
//	kling db class reset [-prefix alumno] [<copia>...] datos nuevos (todas o esas)
//	kling db class rm [-prefix alumno] [<copia>...]    las borra (todas o esas)
//
// No es un modelo nuevo: cada copia es un `kling db up` normal (clave propia,
// solo en el host) con la etiqueta kling.db.class=<prefijo>, que es lo que las
// agrupa; reset y rm son los de siempre, con esa etiqueta como filtro. Las
// operaciones van en paralelo con un tope (-parallel).
//
// Las claves no se imprimen nunca. Con -passwords FICHERO se escriben las DSN
// de las copias listas en ese fichero, 0600, escrito aparte y renombrado.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// labelClass agrupa las copias de una clase; su valor es el prefijo.
	labelClass = "kling.db.class"

	defaultClassPrefix = "student"
	classMax           = 200
	classParallelMax   = 16
	defaultParallel    = 4
)

// classPattern es un prefijo: cabe en una etiqueta (api.KeyPattern) y, con
// "-NNN" detrás, en un nombre de máquina.
var classPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

const classUsage = "usage: kling db class -n N [-prefix P] [-ttl D] [-parallel K] [-passwords FILE] <golden>\n" +
	"       kling db class ls [-prefix P] [-json] [-passwords FILE]\n" +
	"       kling db class reset [-prefix P] [-parallel K] [-passwords FILE] [<copy>...]\n" +
	"       kling db class rm [-prefix P] [-parallel K] [<copy>...]"

func cmdClass(args []string) error {
	fs, host, owner := newFlags("class")
	n := fs.Int("n", 0, fmt.Sprintf("how many copies to create (1-%d)", classMax))
	prefix := fs.String("prefix", defaultClassPrefix, "name of the class: the copies are <prefix>-01, <prefix>-02...")
	ttl := fs.Duration("ttl", 0, "lifetime of each new copy (freezes when it runs out; 0 = none)")
	parallel := fs.Int("parallel", defaultParallel, fmt.Sprintf("how many copies are created, reset or removed at once (1-%d)", classParallelMax))
	passwords := fs.String("passwords", "", "write NAME<TAB>DSN (with the password) of every ready copy to this file, mode 0600; nothing is printed")
	asJSON := fs.Bool("json", false, "with ls: JSON output (no passwords)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	sub := "up"
	if len(pos) > 0 {
		switch pos[0] {
		case "up", "ls", "reset", "rm":
			sub, pos = pos[0], pos[1:]
		}
	}
	switch {
	case sub == "up" && (len(pos) != 1 || *n == 0 || *asJSON),
		sub != "up" && (*n != 0 || *ttl != 0),
		sub == "ls" && len(pos) > 0,
		sub != "ls" && *asJSON,
		sub == "rm" && *passwords != "":
		return usageErr("%s", classUsage)
	}
	if !classPattern.MatchString(*prefix) {
		return fmt.Errorf("invalid -prefix %q: lowercase letters, digits, '_' and '-', up to 40", *prefix)
	}
	if *parallel < 1 || *parallel > classParallelMax {
		return fmt.Errorf("-parallel must be between 1 and %d", classParallelMax)
	}
	if err := validOwner(*owner); err != nil {
		return err
	}
	if *passwords == "-" {
		return errors.New("-passwords needs a file: passwords are never printed")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	o := classOpts{prefix: *prefix, owner: *owner, parallel: *parallel, passwords: *passwords}
	switch sub {
	case "ls":
		return a.classLs(ctx, o, *asJSON)
	case "reset":
		return a.classReset(ctx, o, pos)
	case "rm":
		return a.classRm(ctx, o, pos)
	}
	return a.classUp(ctx, o, pos[0], *n, *ttl)
}

// classOpts es lo común a los subcomandos.
type classOpts struct {
	prefix, owner string
	parallel      int
	passwords     string
}

// classNames son los nombres de las n copias: <prefijo>-01 ... con el ancho
// que haga falta (dos cifras como mínimo, para que ordenen bien).
func classNames(prefix string, n int) []string {
	w := max(2, len(strconv.Itoa(n)))
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%0*d", prefix, w, i+1)
	}
	return names
}

// machines es `kling ps -json`.
func (a *app) machines(ctx context.Context) ([]*api.Machine, error) {
	out, err := a.k.Run(ctx, nil, "ps", "-json")
	if err != nil {
		return nil, err
	}
	if len(out) > maxJSON {
		return nil, errors.New("kling ps: output too large")
	}
	var all []*api.Machine
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("kling ps: %w", err)
	}
	return all, nil
}

// inClass dice si mc es una copia de la clase y el dueño de o.
func (o classOpts) inClass(mc *api.Machine) bool {
	return mc != nil && mc.Labels[labelGolden] != "" && mc.Labels[labelClass] == o.prefix && mc.Labels[labelOwner] == o.owner
}

// classMembers son las copias de la clase, por nombre.
func (a *app) classMembers(ctx context.Context, o classOpts) ([]*api.Machine, error) {
	all, err := a.machines(ctx)
	if err != nil {
		return nil, err
	}
	var out []*api.Machine
	for _, mc := range all {
		if o.inClass(mc) {
			out = append(out, mc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pick elige, de las copias de la clase, las que nombra names (todas si no
// nombra ninguna). Un nombre que no es de la clase es un error: reset y rm no
// tocan nada fuera de ella.
func (o classOpts) pick(members []*api.Machine, names []string) ([]*api.Machine, error) {
	if len(names) == 0 {
		if len(members) == 0 {
			return nil, fmt.Errorf("class %s has no copies (owner %s)", o.prefix, o.owner)
		}
		return members, nil
	}
	byName := map[string]*api.Machine{}
	for _, mc := range members {
		byName[mc.Name] = mc
	}
	var out []*api.Machine
	seen := map[string]bool{}
	for _, n := range names {
		mc, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("%s is not a copy of class %s (owner %s)", n, o.prefix, o.owner)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, mc)
		}
	}
	return out, nil
}

// lockedWriter serializa las escrituras de varias goroutines en un mismo
// io.Writer (los avisos de up en paralelo).
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// bounded ejecuta fn(0..n-1) con como mucho k a la vez y devuelve el error de
// cada una. Si ctx acaba, las que no empezaron devuelven ctx.Err().
func bounded(ctx context.Context, n, k int, fn func(i int) error) []error {
	errs := make([]error, n)
	sem := make(chan struct{}, k)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			errs[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = fn(i)
		}(i)
	}
	wg.Wait()
	return errs
}

// parallelApp es una copia de a cuyas salidas admiten varias goroutines.
func (a *app) parallelApp() *app {
	cp := *a
	cp.stderr = &lockedWriter{w: a.stderr}
	cp.stdout = &lockedWriter{w: a.stdout}
	return &cp
}

// failures junta los errores de una operación en paralelo en uno solo, con el
// nombre de cada copia.
func failures(verb string, names []string, errs []error) error {
	var msgs []string
	for i, err := range errs {
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("  %s: %v", names[i], err))
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d copies could not be %s:\n%s", len(msgs), len(names), verb, strings.Join(msgs, "\n"))
}

// ── up ───────────────────────────────────────────────────────────────────────

// classUp crea las copias que falten de <prefijo>-01 ... <prefijo>-n. Las que
// ya existen en la clase, listas y del mismo golden, se dejan como están: si
// algo falla, repetir el comando crea solo las que faltan.
func (a *app) classUp(ctx context.Context, o classOpts, golden string, n int, ttl time.Duration) error {
	if n < 1 || n > classMax {
		return fmt.Errorf("-n must be between 1 and %d", classMax)
	}
	if !namePattern.MatchString(golden) {
		return fmt.Errorf("invalid template name %q", golden)
	}
	if ttl < 0 || (ttl > 0 && ttl < time.Second) {
		return errors.New("-ttl must be at least 1s")
	}
	// La plantilla, una vez y antes de nada: un golden que no existe no lanza
	// n creaciones que fallan igual.
	if _, err := a.template(ctx, golden); err != nil {
		return err
	}
	all, err := a.machines(ctx)
	if err != nil {
		return err
	}
	byName := map[string]*api.Machine{}
	for _, mc := range all {
		if mc != nil {
			byName[mc.Name] = mc
		}
	}
	var todo, kept []string
	var problems []string
	for _, name := range classNames(o.prefix, n) {
		mc, exists := byName[name]
		switch {
		case !exists:
			todo = append(todo, name)
		case !o.inClass(mc):
			problems = append(problems, fmt.Sprintf("  %s: the name is taken by a machine that is not a copy of class %s (owner %s)", name, o.prefix, o.owner))
		case mc.Labels[labelGolden] != golden:
			problems = append(problems, fmt.Sprintf("  %s: already in the class, from template %s (kling db class rm -prefix %s %s)", name, mc.Labels[labelGolden], o.prefix, name))
		case mc.Labels[labelState] != stateReady:
			problems = append(problems, fmt.Sprintf("  %s: in the class but not ready (kling db class reset -prefix %s %s)", name, o.prefix, name))
		default:
			kept = append(kept, name)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("nothing was created:\n%s", strings.Join(problems, "\n"))
	}
	if len(kept) > 0 {
		fmt.Fprintf(a.stderr, "%d of %d copies already exist; creating the other %d\n", len(kept), n, len(todo))
	}
	pa := a.parallelApp()
	extra := [][2]string{{labelClass, o.prefix}}
	errs := bounded(ctx, len(todo), o.parallel, func(i int) error {
		_, err := pa.upFrom(ctx, golden, golden, todo[i], ttl, o.owner, extra)
		return err
	})
	ferr := failures("created", todo, errs)
	if lerr := a.classReport(ctx, o); lerr != nil && ferr == nil {
		return lerr
	}
	if ferr != nil {
		return fmt.Errorf("%w\nthe others are ready; run the same command again to create only the missing ones", ferr)
	}
	return nil
}

// ── ls ───────────────────────────────────────────────────────────────────────

// classRow es una fila de class ls (y de su -json): nunca lleva la clave.
type classRow struct {
	Name         string `json:"name"`
	Machine      string `json:"machine"`
	Golden       string `json:"golden"`
	State        string `json:"state"`
	Ready        bool   `json:"ready"`
	Host         string `json:"host,omitempty"`
	Port         int    `json:"port,omitempty"`
	User         string `json:"user,omitempty"`
	Database     string `json:"database,omitempty"`
	PasswordFile string `json:"password_file,omitempty"`
}

func classRowOf(mc *api.Machine) classRow {
	r := classRow{Name: mc.Name, Machine: shortID(mc.ID), Golden: mc.Labels[labelGolden], State: string(mc.State)}
	// Lista y con clave de ESTE id en el host, como exige connect (SQLite no
	// tiene clave: le basta con estar lista).
	eng := engineOf(mc.Labels)
	r.Ready = mc.Labels[labelState] == stateReady && (!hasPassword(eng) || dbstate.HasPassword(mc.ID) == nil)
	if role, db, err := roleDB(mc.Labels); err == nil {
		r.User, r.Database = role, db
	}
	if h, p, err := hostAddr(mc); err == nil {
		r.Host, r.Port = h, p
	}
	if r.Ready && hasPassword(eng) {
		r.PasswordFile, _ = dbstate.PasswordPath(mc.ID)
	}
	return r
}

func (a *app) classLs(ctx context.Context, o classOpts, asJSON bool) error {
	members, err := a.classMembers(ctx, o)
	if err != nil {
		return err
	}
	if asJSON {
		rows := make([]classRow, 0, len(members))
		for _, mc := range members {
			rows = append(rows, classRowOf(mc))
		}
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return err
		}
		return a.classPasswords(o, members)
	}
	return a.printClass(o, members)
}

// classReport vuelve a leer la clase, la imprime y escribe -passwords.
func (a *app) classReport(ctx context.Context, o classOpts) error {
	members, err := a.classMembers(ctx, o)
	if err != nil {
		return err
	}
	return a.printClass(o, members)
}

func (a *app) printClass(o classOpts, members []*api.Machine) error {
	if len(members) == 0 {
		fmt.Fprintf(a.stdout, "class %s has no copies (kling db class -n N -prefix %s <golden>)\n", o.prefix, o.prefix)
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "COPY\tSTATE\tHOST\tPORT\tUSER\tDATABASE\tTEMPLATE")
	ready := 0
	for _, mc := range members {
		r := classRowOf(mc)
		st := r.State
		if r.Ready {
			ready++
		} else {
			st += " (not ready)"
		}
		host, port := "-", "-"
		if r.Host != "" {
			host = r.Host
		}
		if r.Port > 0 {
			port = strconv.Itoa(r.Port)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, st, host, port, orDefault(r.User, "-"), orDefault(r.Database, "-"), r.Golden)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%d copies in class %s, %d ready.\n", len(members), o.prefix, ready)
	// La pista con el cliente de cada motor de la clase (una clase sale de
	// una plantilla, pero se listan por prefijo).
	clientes := map[string]bool{}
	for _, mc := range members {
		clientes[clientMode(engineOf(mc.Labels))] = true
	}
	for _, c := range []string{"psql", "mysql", "redis", "sqlite"} {
		if !clientes[c] {
			continue
		}
		if c == "sqlite" {
			fmt.Fprintf(a.stdout, "  one student:       kling db connect <copy> -sqlite\n")
		} else {
			fmt.Fprintf(a.stdout, "  one student:       kling db connect <copy> [-%s | -dsn]\n", c)
		}
	}
	if o.passwords == "" {
		fmt.Fprintf(a.stdout, "  passwords:         not printed; kling db class ls -prefix %s -passwords FILE writes them to a 0600 file\n", o.prefix)
		return nil
	}
	return a.classPasswords(o, members)
}

// classPasswords escribe, si se pidió, NOMBRE<TAB>DSN de cada copia lista en
// o.passwords (0600, aparte y renombrado). Por pantalla solo sale la ruta.
func (a *app) classPasswords(o classOpts, members []*api.Machine) error {
	if o.passwords == "" {
		return nil
	}
	if st, err := os.Stat(o.passwords); err == nil && st.IsDir() {
		return fmt.Errorf("-passwords %s is a directory", o.passwords)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# kling db class %s: one line per ready copy, NAME<TAB>DSN. Secret: every line holds a password.\n", o.prefix)
	n := 0
	for _, mc := range members {
		if classRowOf(mc).Ready {
			if engineOf(mc.Labels) == engineSQLite {
				// Sin servidor de red ni clave: se entra con connect -sqlite.
				fmt.Fprintf(&b, "%s\t(sqlite: kling db connect %s -sqlite)\n", mc.Name, mc.Name)
				continue
			}
			dsn, err := copyDSN(mc)
			if err != nil {
				return fmt.Errorf("%s: %w", mc.Name, err)
			}
			fmt.Fprintf(&b, "%s\t%s\n", mc.Name, dsn)
			n++
		}
	}
	if err := dbstate.WritePrivate(o.passwords, []byte(b.String())); err != nil {
		return fmt.Errorf("writing %s: %w", o.passwords, err)
	}
	fmt.Fprintf(a.stderr, "wrote the passwords of %d copies to %s (mode 0600)\n", n, o.passwords)
	return nil
}

// copyDSN es la DSN, con la clave, de una copia lista: la misma que da
// kling db connect -dsn.
func copyDSN(mc *api.Machine) (string, error) {
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return "", err
	}
	h, port, err := hostAddr(mc)
	if err != nil {
		return "", err
	}
	pw, err := dbstate.ReadPassword(mc.ID)
	if err != nil {
		return "", err
	}
	return engineDSN(engineOf(mc.Labels), role, pw, h, port, db), nil
}

// engineDSN es la DSN de una copia según su motor (SQLite no tiene: no hay
// servidor de red). La usan connect -dsn y class -passwords, para que no
// puedan dar esquemas distintos.
func engineDSN(engine, role, pw, h string, port int, db string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(role, pw),
		Host: net.JoinHostPort(h, strconv.Itoa(port)), Path: "/" + db, RawQuery: "sslmode=disable"}
	switch engine {
	case engineMySQL:
		u.Scheme, u.RawQuery = "mysql", ""
	case engineRedis:
		u.Scheme, u.RawQuery, u.Path = "redis", "", "/0"
	}
	return u.String()
}

// ── reset y rm ───────────────────────────────────────────────────────────────

func (a *app) classReset(ctx context.Context, o classOpts, names []string) error {
	members, err := a.classMembers(ctx, o)
	if err != nil {
		return err
	}
	sel, err := o.pick(members, names)
	if err != nil {
		return err
	}
	pa := a.parallelApp()
	list := make([]string, len(sel))
	for i, mc := range sel {
		list[i] = mc.Name
	}
	errs := bounded(ctx, len(sel), o.parallel, func(i int) error {
		// Por id: si entretanto alguien reutilizó el nombre, no se toca.
		_, err := pa.reset(ctx, sel[i].ID, o.owner)
		return err
	})
	ferr := failures("reset", list, errs)
	if lerr := a.classReport(ctx, o); lerr != nil && ferr == nil {
		return lerr
	}
	return ferr
}

func (a *app) classRm(ctx context.Context, o classOpts, names []string) error {
	members, err := a.classMembers(ctx, o)
	if err != nil {
		return err
	}
	sel, err := o.pick(members, names)
	if err != nil {
		return err
	}
	pa := a.parallelApp()
	list := make([]string, len(sel))
	for i, mc := range sel {
		list[i] = mc.Name
	}
	errs := bounded(ctx, len(sel), o.parallel, func(i int) error {
		if err := pa.remove(ctx, sel[i]); err != nil {
			return err
		}
		fmt.Fprintln(pa.stdout, sel[i].Name)
		return nil
	})
	return failures("removed", list, errs)
}
