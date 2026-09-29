package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/pgmini"
)

// target es a dónde conecta el cliente para consultar una copia.
type target struct {
	Addr, User, Password, Database string
}

// backend es una forma de conseguir "una base de datos lista con el esquema y
// los datos". Las tres son el mismo trabajo visto desde el test: pedirla y
// poder consultar el seed.
type backend interface {
	name() string
	// setup y teardown: una vez por ejecución, fuera de la medida.
	setup(ctx context.Context) error
	teardown(ctx context.Context)
	// prepare deja listo lo que NO forma parte de la medida de una ronda de n
	// copias (p. ej. la máquina origen del fork).
	prepare(ctx context.Context, rep, n int) (round, error)
}

// round es una ronda de n copias en paralelo.
type round interface {
	// provision entrega la copia i: cuando vuelve, la base existe y tiene el
	// esquema y los datos (o eso pretende; el banco lo comprueba con la
	// primera consulta). extraMS es un tiempo que la herramienta informa por su
	// cuenta (thaw_ms de kling), 0 si no hay.
	provision(ctx context.Context, i int) (t target, extraMS int, err error)
	// cleanup borra todo lo que creó la ronda. Debe poder llamarse aunque la
	// ronda haya fallado a medias.
	cleanup(ctx context.Context)
}

// cmdRunner lanza herramientas externas; es un campo para poder sustituirlo
// en pruebas.
type cmdRunner func(ctx context.Context, env []string, name string, args ...string) (string, error)

// runCmd ejecuta name con args y devuelve stdout. Si falla, el error lleva el
// principio de stderr. env se AÑADE al del proceso: así las contraseñas viajan
// por entorno y nunca por argv (visible en ps).
func runCmd(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(se.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return so.String(), fmt.Errorf("%s %s: %w: %s", name, firstArg(args), err, msg)
	}
	return so.String(), nil
}

func firstArg(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

// ---- kindling: kling run -from <golden> ----

type klingBackend struct {
	cfg  *config
	run  cmdRunner
	fork bool // false: run -from; true: sandbox fork
}

func (b *klingBackend) name() string {
	if b.fork {
		return "kindling-fork"
	}
	return "kindling-run"
}
func (b *klingBackend) setup(ctx context.Context) error {
	if b.fork {
		if b.cfg.ForkSrc == "" {
			return errors.New("kindling-fork needs -fork-src <machine>")
		}
		return nil
	}
	if b.cfg.Golden == "" {
		return errors.New("kindling-run needs -golden <template>")
	}
	return nil
}
func (b *klingBackend) teardown(context.Context) {}

func (b *klingBackend) prepare(_ context.Context, rep, n int) (round, error) {
	return &klingRound{b: b, prefix: fmt.Sprintf("%s_%s_n%d_r%d", b.cfg.Prefix, b.cfg.RunID, n, rep), n: n}, nil
}

type klingRound struct {
	b      *klingBackend
	prefix string
	n      int

	mu    sync.Mutex
	refs  []string // lo que hay que borrar
	once  sync.Once
	forks []target
	ferr  error
	fms   int
}

func (r *klingRound) track(ref string) {
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	r.mu.Unlock()
}

func (r *klingRound) provision(ctx context.Context, i int) (target, int, error) {
	c := r.b.cfg
	if r.b.fork {
		// Un solo `sandbox fork -n N`: todas las copias dependen de esa
		// llamada, así que su tiempo entra en la medida de cada una.
		r.once.Do(func() { r.forkAll(ctx) })
		if r.ferr != nil {
			return target{}, 0, r.ferr
		}
		if i >= len(r.forks) {
			return target{}, 0, fmt.Errorf("fork returned %d copies, wanted %d", len(r.forks), r.n)
		}
		return r.forks[i], r.fms, nil
	}
	name := fmt.Sprintf("%s-%d", r.prefix, i)
	// Se apunta ANTES de crear: si `run` falla a medias, cleanup lo intenta
	// borrar igual (rm -f de algo que no existe no es un problema).
	r.track(name)
	args := []string{"run", "-from", c.Golden, "-name", name}
	args = append(args, c.KlingRunArgs...)
	out, err := r.b.run(ctx, nil, c.Kling, args...)
	if err != nil {
		return target{}, 0, err
	}
	ri, err := parseRunOutput(out)
	if err != nil {
		return target{}, 0, err
	}
	addr, err := r.addr(ctx, name)
	if err != nil {
		return target{}, 0, err
	}
	return c.target(addr), ri.Ms, nil
}

func (r *klingRound) addr(ctx context.Context, ref string) (string, error) {
	out, err := r.b.run(ctx, nil, r.b.cfg.Kling, "inspect", ref)
	if err != nil {
		return "", err
	}
	return parseInspect([]byte(out), r.b.cfg.PGPort)
}

func (r *klingRound) forkAll(ctx context.Context) {
	c := r.b.cfg
	start := time.Now()
	out, err := r.b.run(ctx, nil, c.Kling, "sandbox", "fork", c.ForkSrc, "-n", strconv.Itoa(r.n), "-json")
	if err != nil {
		r.ferr = err
		return
	}
	res, err := parseFork([]byte(out))
	if err != nil {
		r.ferr = err
		return
	}
	r.fms = int(time.Since(start).Milliseconds())
	for _, m := range res.Sandboxes {
		r.track(m.ID)
		if !m.Reachable() {
			r.forks = append(r.forks, target{}) // se notará al conectar
			continue
		}
		r.forks = append(r.forks, c.target(m.Addr(c.PGPort)))
	}
}

func (r *klingRound) cleanup(ctx context.Context) {
	r.mu.Lock()
	refs := r.refs
	r.refs = nil
	r.mu.Unlock()
	if len(refs) == 0 {
		return
	}
	// rm -f: sin confirmación, y tolera lo que no exista.
	args := append([]string{"rm", "-f"}, refs...)
	if _, err := r.b.run(ctx, nil, r.b.cfg.Kling, args...); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cleanup kling rm: %v\n", err)
	}
}

// ---- docker: docker run + esperar + esquema y datos ----

type dockerBackend struct {
	cfg *config
	run cmdRunner
	pw  string
}

func (b *dockerBackend) name() string { return "docker" }
func (b *dockerBackend) setup(ctx context.Context) error {
	// La imagen se baja aquí, fuera de la medida: lo que se compara es el
	// arranque de la base, no la red.
	if _, err := b.run(ctx, nil, b.cfg.Docker, "image", "inspect", b.cfg.DockerImage); err != nil {
		if _, err := b.run(ctx, nil, b.cfg.Docker, "pull", "-q", b.cfg.DockerImage); err != nil {
			return err
		}
	}
	var r [12]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	b.pw = hex.EncodeToString(r[:])
	return nil
}
func (b *dockerBackend) teardown(ctx context.Context) {
	dockerRm(ctx, b.run, b.cfg, b.cfg.Prefix+"_"+b.cfg.RunID+"_")
}

// dockerRm borra los contenedores cuyo nombre empieza por prefix.
func dockerRm(ctx context.Context, run cmdRunner, c *config, prefix string) {
	out, err := run(ctx, nil, c.Docker, "ps", "-aq", "--filter", "name=^"+prefix)
	if err != nil {
		return
	}
	if ids := strings.Fields(out); len(ids) > 0 {
		_, _ = run(ctx, nil, c.Docker, append([]string{"rm", "-f", "-v"}, ids...)...)
	}
}

func (b *dockerBackend) prepare(_ context.Context, rep, n int) (round, error) {
	return &dockerRound{b: b, prefix: fmt.Sprintf("%s_%s_n%d_r%d", b.cfg.Prefix, b.cfg.RunID, n, rep)}, nil
}

type dockerRound struct {
	b      *dockerBackend
	prefix string
}

func (r *dockerRound) provision(ctx context.Context, i int) (target, int, error) {
	c := r.b.cfg
	name := fmt.Sprintf("%s-%d", r.prefix, i)
	// Publicado solo en loopback y en puerto efímero; la contraseña por
	// entorno (-e VAR sin valor toma el del proceso).
	_, err := r.b.run(ctx, []string{"POSTGRES_PASSWORD=" + r.b.pw}, c.Docker,
		"run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::5432",
		"-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_USER="+c.User, "-e", "POSTGRES_DB="+c.DB,
		c.DockerImage)
	if err != nil {
		return target{}, 0, err
	}
	out, err := r.b.run(ctx, nil, c.Docker, "port", name, "5432/tcp")
	if err != nil {
		return target{}, 0, err
	}
	addr, err := parseDockerPort(out)
	if err != nil {
		return target{}, 0, err
	}
	t := target{Addr: addr, User: c.User, Password: r.b.pw, Database: c.DB}
	// Lo que haría Testcontainers: esperar a que acepte conexiones y aplicar
	// el esquema y los datos. Mientras la imagen inicializa, el servidor
	// temporal no escucha por TCP, así que un connect que funciona ya es el
	// definitivo; aun así se reintenta la aplicación completa.
	if err := waitAndSeed(ctx, t, c.seedSQL()); err != nil {
		return target{}, 0, err
	}
	return t, 0, nil
}

func (r *dockerRound) cleanup(ctx context.Context) {
	dockerRm(ctx, r.b.run, r.b.cfg, r.prefix+"-")
}

// waitAndSeed reintenta hasta que la base acepta conexiones y el SQL se aplica.
func waitAndSeed(ctx context.Context, t target, sql string) error {
	var last error
	for {
		conn, err := pgmini.Dial(ctx, pgmini.Config{Addr: t.Addr, User: t.User, Password: t.Password, Database: t.Database})
		if err == nil {
			_, err = conn.Query(ctx, sql)
			conn.Close()
			if err == nil {
				return nil
			}
			var pe *pgmini.Error
			if errors.As(err, &pe) {
				return fmt.Errorf("seed: %w", err) // el SQL falló: reintentar no lo arregla
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the database: %w (last: %v)", ctx.Err(), last)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// ---- template: CREATE DATABASE ... TEMPLATE en un Postgres ya arrancado ----

type templateBackend struct {
	cfg *config
	tpl string
}

func (b *templateBackend) name() string { return "template" }

func (b *templateBackend) admin(ctx context.Context) (*pgmini.Conn, error) {
	c := b.cfg
	return pgmini.Dial(ctx, pgmini.Config{Addr: c.TemplateAddr, User: c.User, Password: c.password(), Database: c.TemplateAdminDB})
}

func (b *templateBackend) setup(ctx context.Context) error {
	if b.cfg.TemplateAddr == "" {
		return errors.New("template needs -template-addr host:port")
	}
	b.tpl = fmt.Sprintf("%s_%s_tpl", b.cfg.Prefix, b.cfg.RunID)
	a, err := b.admin(ctx)
	if err != nil {
		return err
	}
	_, err = a.Query(ctx, "CREATE DATABASE "+b.tpl)
	a.Close()
	if err != nil {
		return err
	}
	t := target{Addr: b.cfg.TemplateAddr, User: b.cfg.User, Password: b.cfg.password(), Database: b.tpl}
	return waitAndSeed(ctx, t, b.cfg.seedSQL())
}

func (b *templateBackend) teardown(ctx context.Context) {
	if b.tpl == "" {
		return
	}
	dropDB(ctx, b, b.tpl)
}

func dropDB(ctx context.Context, b *templateBackend, db string) {
	a, err := b.admin(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cleanup drop %s: %v\n", db, err)
		return
	}
	defer a.Close()
	if _, err := a.Query(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cleanup drop %s: %v\n", db, err)
	}
}

func (b *templateBackend) prepare(_ context.Context, rep, n int) (round, error) {
	return &templateRound{b: b, prefix: fmt.Sprintf("%s_%s_n%d_r%d", b.cfg.Prefix, b.cfg.RunID, n, rep), n: n}, nil
}

type templateRound struct {
	b      *templateBackend
	prefix string
	n      int
}

func (r *templateRound) dbName(i int) string { return fmt.Sprintf("%s_%d", r.prefix, i) }

func (r *templateRound) provision(ctx context.Context, i int) (target, int, error) {
	c := r.b.cfg
	a, err := r.b.admin(ctx)
	if err != nil {
		return target{}, 0, err
	}
	name := r.dbName(i)
	_, err = a.Query(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, r.b.tpl))
	a.Close()
	if err != nil {
		return target{}, 0, err
	}
	return target{Addr: c.TemplateAddr, User: c.User, Password: c.password(), Database: name}, 0, nil
}

func (r *templateRound) cleanup(ctx context.Context) {
	for i := 0; i < r.n; i++ {
		dropDB(ctx, r.b, r.dbName(i))
	}
}
