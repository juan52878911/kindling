package main

// kling db branch: una base de datos por rama de git.
//
// Cada rama del repositorio tiene su copia (una microVM de kling db). La copia
// de una rama nueva sale de la de su rama padre (fork: los datos y el esquema
// de esa rama, en milisegundos) o, si no la hay, del golden. Al cambiar de
// rama (hook post-checkout → -switch) la copia de la rama activa se descongela
// y las de las demás ramas del repo se congelan: 0 RAM, y thaw en ~10 ms al
// volver.
//
// Identidad. Una copia de rama se reconoce por sus etiquetas, no por su nombre:
// sandbox fork no deja poner nombre. kling.db.repo es el hash del directorio
// git común del repo (el mismo en todos sus worktrees) y kling.db.branch la CLAVE de la rama (branchKey), no el nombre: un
// nombre de rama es texto arbitrario (barras, mayúsculas, unicode) y no puede
// ir tal cual a una etiqueta, a un nombre de máquina ni, desde luego, a argv o
// a SQL. La clave es un slug ASCII más un hash corto del nombre entero, así
// que es estable y `feat/x` y `feat-x` no chocan.
//
// Seguridad. La clave de cada copia sigue solo en el host (dbstate). El fichero
// con la conexión para la aplicación va DENTRO de .git (kling-db.env, 0600):
// nunca en el árbol de trabajo, para que no se suba al repo. Solo se actúa
// sobre copias del dueño indicado.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// branchEnvFile es el fichero de conexión, dentro del directorio git.
	branchEnvFile = "kling-db.env"
	maxBranchLen  = 255
	// defaultParent es la rama padre si git no dice cuál es la por defecto.
	defaultParent = "main"
)

// ── nombres ──────────────────────────────────────────────────────────────────

// validBranch acepta lo que git acepta salvo lo que sería peligroso o absurdo
// como argumento: vacío, demasiado largo, UTF-8 inválido, caracteres de control
// y un guion delante (parecería un flag).
func validBranch(b string) error {
	switch {
	case b == "":
		return errors.New("empty branch name")
	case len(b) > maxBranchLen:
		return fmt.Errorf("branch name longer than %d bytes", maxBranchLen)
	case !utf8.ValidString(b):
		return errors.New("branch name is not valid UTF-8")
	case b[0] == '-':
		return fmt.Errorf("invalid branch name %q: it starts with '-'", b)
	}
	for _, r := range b {
		if unicode.IsControl(r) {
			return errors.New("invalid branch name: control characters")
		}
		if bidiRune(r) {
			return errors.New("invalid branch name: bidirectional formatting characters")
		}
	}
	return nil
}

// bidiRune dice si r es una marca o control de dirección de texto (LRM, RLM,
// ALM, embebidos, overrides y aislados): sirven para disfrazar un nombre.
func bidiRune(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F ||
		(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// branchKey es la clave estable de una rama: slug ASCII (hasta 40) + '-' + 6
// hex del hash del nombre. Cumple api.KeyPattern y namePattern.
func branchKey(branch string) string {
	var sb strings.Builder
	dash := true // sin guion inicial
	for _, r := range strings.ToLower(branch) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			dash = false
		} else if !dash {
			sb.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(sb.String(), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = "b"
	}
	h := sha256.Sum256([]byte(branch))
	return slug + "-" + hex.EncodeToString(h[:3])
}

// repoKey identifica un repositorio por su directorio git COMÚN (git
// rev-parse --git-common-dir, ruta real): el mismo para el árbol principal y
// para todos sus worktrees, así que una rama tiene una sola copia aunque se
// abra desde varios. El prefijo separa este hash del de legacyRepoKey.
func repoKey(commonDir string) string {
	if r, err := filepath.EvalSymlinks(commonDir); err == nil {
		commonDir = r
	}
	h := sha256.Sum256([]byte("git-common-dir:" + commonDir))
	return hex.EncodeToString(h[:6])
}

// legacyRepoKey es la clave de antes (hash del toplevel): cada worktree tenía
// la suya. Las copias etiquetadas así se siguen reconociendo desde el árbol
// que las creó; las nuevas llevan repoKey.
func legacyRepoKey(toplevel string) string {
	if r, err := filepath.EvalSymlinks(toplevel); err == nil {
		toplevel = r
	}
	h := sha256.Sum256([]byte(toplevel))
	return hex.EncodeToString(h[:6])
}

// branchMachineName es el nombre con el que se crea la copia de una rama que
// sale del golden (una que sale de un fork la nombra el daemon).
func branchMachineName(repo, key string) string { return "db-" + repo[:6] + "-" + key }

// ── git ──────────────────────────────────────────────────────────────────────

// git ejecuta `git <args>` en a.cwd con argumentos FIJOS: el nombre de una
// rama nunca llega aquí como argumento. Devuelve stdout sin el salto final.
func (a *app) git(ctx context.Context, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = a.cwd
	c.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var out, eb bytes.Buffer
	c.Stdout, c.Stderr = &out, &eb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(eb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// repoInfo es el repositorio en el que estamos.
type repoInfo struct {
	repo     string // kling.db.repo (repoKey del directorio git común)
	legacy   string // la clave de antes (legacyRepoKey del toplevel)
	toplevel string
	gitDir   string // absoluto, el de ESTE worktree (ahí va kling-db.env)
}

// is dice si una etiqueta kling.db.repo es de este repositorio.
func (ri *repoInfo) is(label string) bool {
	return label != "" && (label == ri.repo || label == ri.legacy)
}

func (a *app) repo(ctx context.Context) (*repoInfo, error) {
	top, err := a.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository: %w", err)
	}
	gd, err := a.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	common, err := a.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if top == "" || gd == "" || common == "" {
		return nil, errors.New("git did not say where the repository is")
	}
	return &repoInfo{repo: repoKey(common), legacy: legacyRepoKey(top), toplevel: top, gitDir: gd}, nil
}

// branchLockWait es cuánto espera una operación de branch a que acabe otra del
// mismo repositorio (crear una copia del golden tarda segundos). Variable para
// los tests.
var branchLockWait = 3 * time.Minute

// lockRepo serializa las operaciones de kling db branch de un repositorio en
// este host: dos checkouts a la vez (dos worktrees, o un hook y un comando) no
// crean dos copias de la misma rama ni congelan la que otro acaba de activar.
func (a *app) lockRepo(ctx context.Context, ri *repoInfo) (func(), error) {
	un, err := dbstate.Lock(ctx, "branch-"+ri.repo, branchLockWait, func() {
		fmt.Fprintln(a.stderr, "waiting for another kling db branch on this repository...")
	})
	if errors.Is(err, dbstate.ErrLocked) {
		return nil, fmt.Errorf("another kling db branch on this repository has been running for more than %s", branchLockWait)
	}
	return un, err
}

// worktreeBranches son las claves de las ramas que tiene activas algún
// worktree del repo (git worktree list): esas copias están en uso.
func (a *app) worktreeBranches(ctx context.Context) (map[string]bool, error) {
	out, err := a.git(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if b, ok := strings.CutPrefix(strings.TrimSpace(l), "branch refs/heads/"); ok && validBranch(b) == nil {
			m[branchKey(b)] = true
		}
	}
	return m, nil
}

// currentBranch es la rama activa. HEAD suelto no tiene copia.
func (a *app) currentBranch(ctx context.Context) (string, error) {
	// symbolic-ref (y no rev-parse --abbrev-ref) también funciona en un repo
	// sin commits y falla, con nombre, en HEAD suelto.
	b, err := a.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || b == "" {
		return "", errors.New("HEAD is detached: no branch to give a database to (name one: kling db branch <branch>)")
	}
	return b, validBranch(b)
}

// defaultBranch es la rama por defecto del repo: la de origin/HEAD o main.
func (a *app) defaultBranch(ctx context.Context) string {
	ref, err := a.git(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if b := strings.TrimPrefix(ref, "origin/"); b != "" && validBranch(b) == nil {
			return b
		}
	}
	return defaultParent
}

// localBranches son las ramas locales existentes, como claves.
func (a *app) localBranches(ctx context.Context) (map[string]string, error) {
	out, err := a.git(ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	m := map[string]string{} // clave -> nombre
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && validBranch(l) == nil {
			m[branchKey(l)] = l
		}
	}
	return m, nil
}

// ── copias de un repo ────────────────────────────────────────────────────────

// repoCopies son las copias de rama de este repo (con su clave actual o la de
// antes) y dueño, por clave de rama.
func (a *app) repoCopies(ctx context.Context, ri *repoInfo, owner string) (map[string][]*api.Machine, error) {
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
	m := map[string][]*api.Machine{}
	for _, mc := range all {
		if mc == nil || !ri.is(mc.Labels[labelRepo]) || mc.Labels[labelGolden] == "" || mc.Labels[labelOwner] != owner {
			continue
		}
		k := mc.Labels[labelBranch]
		if k == "" || !api.KeyPattern.MatchString(k) {
			continue
		}
		m[k] = append(m[k], mc)
	}
	return m, nil
}

// one da la copia de una clave, o nil. Dos copias de la misma rama no deberían
// existir (carrera de dos checkouts): se niega a elegir una.
func one(copies map[string][]*api.Machine, key string) (*api.Machine, error) {
	switch l := copies[key]; len(l) {
	case 0:
		return nil, nil
	case 1:
		return l[0], nil
	default:
		names := make([]string, len(l))
		for i, mc := range l {
			names[i] = mc.Name
		}
		return nil, fmt.Errorf("several copies for branch key %s (%s): remove the extra ones with kling db rm", key, strings.Join(names, ", "))
	}
}

func (a *app) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// usable dice si una copia sirve de origen de un fork o de conexión: lista,
// propia y con clave en este host.
func usable(mc *api.Machine, owner string) bool {
	if owned(mc, owner) != nil || mc.Labels[labelState] != stateReady {
		return false
	}
	return dbstate.HasPassword(mc.ID) == nil
}

// ensureBranch devuelve la copia de la rama, creándola si no existe: fork de la
// de from (si existe y está lista) o up del golden.
func (a *app) ensureBranch(ctx context.Context, ri *repoInfo, branch, from, golden, owner string) (*api.Machine, bool, error) {
	key := branchKey(branch)
	copies, err := a.repoCopies(ctx, ri, owner)
	if err != nil {
		return nil, false, err
	}
	if mc, err := one(copies, key); err != nil {
		return nil, false, err
	} else if mc != nil {
		if mc.Labels[labelState] != stateReady {
			return nil, false, fmt.Errorf("the copy %s of branch %s is not ready: remove it (kling db branch -rm %s) and try again", mc.Name, branch, branch)
		}
		return mc, false, nil
	}

	extra := [][2]string{{labelRepo, ri.repo}, {labelBranch, key}, {labelUsed, strconv.FormatInt(a.clock().Unix(), 10)}}
	parentKey := branchKey(from)
	var parent *api.Machine
	if parentKey != key {
		if parent, err = one(copies, parentKey); err != nil {
			return nil, false, err
		}
	}
	if parent != nil && usable(parent, owner) {
		wasFrozen := parent.State == api.StateWarm || parent.State == api.StatePaused
		fmt.Fprintf(a.stderr, "branch %s: forking the copy of %s...\n", branch, from)
		cs, err := a.forkWith(ctx, parent.ID, 1, owner, extra)
		if wasFrozen {
			// fork descongeló al padre; una rama que no es la activa no gasta RAM.
			if _, ferr := a.k.Run(ctx, nil, "freeze", parent.ID); ferr != nil {
				fmt.Fprintf(a.stderr, "warning: could not freeze %s again: %v\n", parent.Name, ferr)
			}
		}
		if err != nil {
			return nil, false, err
		}
		return cs[0], true, nil
	}

	if golden == "" {
		golden = a.repoGolden(copies)
	}
	if golden == "" {
		return nil, false, fmt.Errorf("no ready copy of %s to fork and no golden to start from: kling db branch -golden <template>", from)
	}
	if parent == nil {
		fmt.Fprintf(a.stderr, "branch %s: no copy of %s; starting from the golden %s\n", branch, from, golden)
	} else {
		fmt.Fprintf(a.stderr, "branch %s: the copy of %s is not ready; starting from the golden %s\n", branch, from, golden)
	}
	mc, err := a.upFrom(ctx, golden, golden, branchMachineName(ri.repo, key), 0, owner, extra)
	if err != nil {
		return nil, false, err
	}
	return mc, true, nil
}

// repoGolden es el golden de cualquier copia del repo (para un hook sin -golden).
func (a *app) repoGolden(copies map[string][]*api.Machine) string {
	var names []string
	for _, l := range copies {
		for _, mc := range l {
			if g := mc.Labels[labelGolden]; namePattern.MatchString(g) {
				names = append(names, g)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// activate deja la copia corriendo y devuelve su estado al día (la IP o el
// reenvío pueden cambiar al descongelar).
func (a *app) activate(ctx context.Context, mc *api.Machine, owner string) (*api.Machine, error) {
	switch mc.State {
	case api.StateRunning:
	case api.StateWarm, api.StatePaused:
		if _, err := a.k.Run(ctx, nil, "thaw", mc.ID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%s is %s: remove it (kling db branch -rm) and create it again", mc.Name, mc.State)
	}
	cur, err := a.inspect(ctx, mc.ID)
	if err != nil {
		return nil, err
	}
	if err := checkReady(cur, owner); err != nil {
		return nil, err
	}
	// Mejor esfuerzo: solo alimenta la columna "last used" de -ls.
	if err := a.k.SetLabels(ctx, cur.ID, map[string]string{labelUsed: strconv.FormatInt(a.clock().Unix(), 10)}); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not record the last use of %s: %v\n", cur.Name, err)
	}
	return cur, nil
}

// ── comando ──────────────────────────────────────────────────────────────────

func cmdBranch(args []string) error {
	fs, host, owner := newFlags("branch")
	from := fs.String("from", "", "parent branch whose copy the new one is forked from (default: the repo's default branch)")
	golden := fs.String("golden", "", "template to start from when there is no parent copy to fork")
	envTpl := fs.String("env", "", "instead of a single copy, bring up the branch's whole environment: a graph of this app template + a database copy (see kling db env)")
	sw := fs.Bool("switch", false, "for the git hook: activate this branch's copy, freeze the others, write "+branchEnvFile+" in .git")
	ls := fs.Bool("ls", false, "list the branches with their copy")
	rm := fs.String("rm", "", "remove the copy of this branch")
	prune := fs.Bool("prune", false, "remove the copies of branches that no longer exist in git")
	dry := fs.Bool("dry-run", false, "with -prune: only say what would be removed")
	asJSON := fs.Bool("json", false, "with -ls: JSON output")
	force := fs.Bool("force", false, "with hook install: install even if core.hooksPath points into the working tree or outside the repo")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	modes := 0
	for _, on := range []bool{*sw, *ls, *rm != "", *prune} {
		if on {
			modes++
		}
	}
	const usage = "usage: kling db branch [<branch>] [-from P] [-golden G] | -switch [-golden G] | -ls [-json] | -rm <branch> | -prune [-dry-run] | [-force] [-owner T] [-golden G] hook install|uninstall"
	if err := validOwner(*owner); err != nil {
		return err
	}
	if *golden != "" && !namePattern.MatchString(*golden) {
		return fmt.Errorf("invalid template name %q", *golden)
	}
	if len(pos) == 2 && pos[0] == "hook" {
		if modes > 0 || *from != "" || *envTpl != "" || *dry || *asJSON || (pos[1] == "uninstall" && *golden != "") {
			return usageErr("%s", usage)
		}
		// Solo lo que se pidió explícitamente va al hook: sin -owner, el dueño
		// por defecto (y el hook sigue sirviendo si ese valor cambia).
		ho := hookOpts{force: *force, golden: *golden}
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "owner" {
				ho.owner = *owner
			}
		})
		return cmdBranchHook(pos[1], ho)
	}
	if modes > 1 || len(pos) > 1 || (len(pos) == 1 && modes > 0) {
		return usageErr("%s", usage)
	}
	if *force {
		return usageErr("%s", usage)
	}
	if *envTpl != "" && (modes > 0 || *from != "" || !namePattern.MatchString(*envTpl)) {
		return usageErr("%s", usage)
	}
	// -golden vale con -switch (el hook lo pasa para una rama sin padre).
	if (*dry && !*prune) || (*asJSON && !*ls) || ((*sw || *ls || *rm != "" || *prune) && *from != "") ||
		((*ls || *rm != "" || *prune) && *golden != "") {
		return usageErr("%s", usage)
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	switch {
	case *sw:
		return a.branchSwitch(ctx, *owner, *golden)
	case *ls:
		return a.branchLs(ctx, *owner, *asJSON)
	case *rm != "":
		return a.branchRm(ctx, *rm, *owner)
	case *prune:
		return a.branchPrune(ctx, *owner, *dry)
	}
	branch := ""
	if len(pos) == 1 {
		branch = pos[0]
		if err := validBranch(branch); err != nil {
			return err
		}
	}
	if *envTpl != "" {
		return a.branchEnv(ctx, branch, *envTpl, *golden, *owner)
	}
	return a.branch(ctx, branch, *from, *golden, *owner)
}

// branchEnvName es el nombre del grafo de una rama: cabe en los 24 del núcleo
// (e + 5 del repo + slug de hasta 8 + hash de 6) y sigue siendo único por rama.
func branchEnvName(repo, branch string) string {
	key := branchKey(branch)
	i := strings.LastIndexByte(key, '-')
	slug, hash := key[:i], key[i+1:]
	if len(slug) > 8 {
		slug = strings.Trim(slug[:8], "-")
	}
	return "e" + repo[:5] + "-" + slug + "-" + hash
}

// branchEnvNames son los nombres que puede tener el grafo de una rama: el de
// la clave del repo y, si difiere, el de la clave de antes.
func branchEnvNames(ri *repoInfo, branch string) []string {
	names := []string{branchEnvName(ri.repo, branch)}
	if ri.legacy != "" && ri.legacy != ri.repo {
		names = append(names, branchEnvName(ri.legacy, branch))
	}
	return names
}

// branchEnv: el entorno entero de una rama (un grafo app + base), creado si no
// existe. Es aparte de la copia suelta de la rama: no la usa ni la congela el
// hook. El golden sale de -golden o de cualquier copia del repo.
func (a *app) branchEnv(ctx context.Context, branch, appTpl, golden, owner string) error {
	ri, branch, _, err := a.resolve(ctx, branch, "")
	if err != nil {
		return err
	}
	un, err := a.lockRepo(ctx, ri)
	if err != nil {
		return err
	}
	defer un()
	// El de la clave de antes, si existe, sigue siendo el de la rama.
	for _, n := range branchEnvNames(ri, branch) {
		if g, err := a.graphOf(ctx, n); err != nil {
			return err
		} else if g != nil {
			fmt.Fprintf(a.stdout, "branch %s  environment %s  ready\n", branch, n)
			return nil
		}
	}
	name := branchEnvName(ri.repo, branch)
	if golden == "" {
		copies, err := a.repoCopies(ctx, ri, owner)
		if err != nil {
			return err
		}
		if golden = a.repoGolden(copies); golden == "" {
			return errors.New("no golden to start the database from: kling db branch -env <app-template> -golden <template>")
		}
	}
	g, err := a.envUp(ctx, envOpts{name: name, appTpl: appTpl, golden: golden, owner: owner})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "branch %s  environment %s  created\n", branch, name)
	a.envReport(g)
	return nil
}

// resolve fija repo, rama y padre de una llamada.
func (a *app) resolve(ctx context.Context, branch, from string) (*repoInfo, string, string, error) {
	ri, err := a.repo(ctx)
	if err != nil {
		return nil, "", "", err
	}
	if branch == "" {
		if branch, err = a.currentBranch(ctx); err != nil {
			return nil, "", "", err
		}
	}
	if from == "" {
		from = a.defaultBranch(ctx)
	} else if err := validBranch(from); err != nil {
		return nil, "", "", fmt.Errorf("-from: %w", err)
	}
	return ri, branch, from, nil
}

// branch: la copia de la rama, creada si no existe, y cómo conectar.
func (a *app) branch(ctx context.Context, branch, from, golden, owner string) error {
	ri, branch, from, err := a.resolve(ctx, branch, from)
	if err != nil {
		return err
	}
	un, err := a.lockRepo(ctx, ri)
	if err != nil {
		return err
	}
	defer un()
	mc, created, err := a.ensureBranch(ctx, ri, branch, from, golden, owner)
	if err != nil {
		return err
	}
	if mc, err = a.activate(ctx, mc, owner); err != nil {
		return err
	}
	verb := "ready"
	if created {
		verb = "created"
	}
	fmt.Fprintf(a.stdout, "branch %s  %s  (copy %s, machine %s)\n", branch, verb, mc.Name, shortID(mc.ID))
	// Si es la rama actual, la app ya puede conectar: se escribe la conexión en
	// .git como haría el hook (antes solo -switch lo hacía y, hasta el primer
	// checkout, la app no tenía con qué conectar; lo vio la prueba en el lab).
	if _, cur, _, err := a.resolve(ctx, "", ""); err == nil && cur == branch {
		envPath := filepath.Join(ri.gitDir, branchEnvFile)
		if err := a.writeBranchEnv(mc, envPath); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "  connection:       %s  (mode 0600, inside .git: it is never committed)\n", envPath)
	}
	fmt.Fprintf(a.stdout, "  from this host:   kling db connect %s [-psql | -dsn]\n", mc.Name)
	fmt.Fprintf(a.stdout, "  for your app:     kling db branch hook install   (then every checkout writes .git/%s)\n", branchEnvFile)
	return nil
}

// branchSwitch es lo que llama el hook: deja activa la copia de la rama actual,
// congela las de las demás ramas del repo que ningún worktree tenga activas y
// escribe la conexión en el .git de este worktree. golden es de dónde sale una
// rama sin copia del padre ("": el de cualquier copia del repo).
func (a *app) branchSwitch(ctx context.Context, owner, golden string) error {
	ri, branch, from, err := a.resolve(ctx, "", "")
	if err != nil {
		return err
	}
	// Dos checkouts a la vez (dos worktrees): el segundo espera al primero y
	// encuentra la copia hecha en vez de crear otra.
	un, err := a.lockRepo(ctx, ri)
	if err != nil {
		return err
	}
	defer un()
	envPath := filepath.Join(ri.gitDir, branchEnvFile)
	// Antes que nada: un .env de la rama anterior apuntaría la aplicación a la
	// base equivocada. Sin él, falla (y se nota) en vez de usar otra rama.
	if err := os.Remove(envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the previous %s: %w", envPath, err)
	}
	mc, _, err := a.ensureBranch(ctx, ri, branch, from, golden, owner)
	if err != nil {
		return err
	}
	if mc, err = a.activate(ctx, mc, owner); err != nil {
		return err
	}
	if err := a.writeBranchEnv(mc, envPath); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "branch %s  active  (copy %s)\n  connection: %s  (mode 0600, inside .git: it is never committed)\n", branch, mc.Name, envPath)

	copies, err := a.repoCopies(ctx, ri, owner)
	if err != nil {
		return fmt.Errorf("freezing the other branches: %w", err)
	}
	// La rama que otro worktree tiene activa está en uso: no se congela. Si git
	// no sabe decirlo, no se congela nada (congelar de más rompe a otro).
	inUse, err := a.worktreeBranches(ctx)
	if err != nil {
		return fmt.Errorf("freezing the other branches: %w", err)
	}
	var failed []string
	for k, l := range copies {
		if k == branchKey(branch) || inUse[k] {
			continue
		}
		for _, o := range l {
			if o.State != api.StateRunning {
				continue
			}
			if _, err := a.k.Run(ctx, nil, "freeze", o.ID); err != nil {
				failed = append(failed, o.Name)
			}
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("could not freeze: %s", strings.Join(failed, ", "))
	}
	return nil
}

// envQuote entrecomilla un valor para el .env: entre comillas simples, que ni
// el shell ni los lectores de .env expanden (una comilla simple se escribe
// '\”). Un valor sin caracteres especiales va tal cual.
func envQuote(v string) string {
	safe := v != ""
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.,:@%+=/", r)) {
			safe = false
			break
		}
	}
	if safe {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// writeBranchEnv escribe la conexión de la copia, 0600, en path (dentro de
// .git), aparte y renombrado: nunca queda a medias ni con permisos abiertos.
func (a *app) writeBranchEnv(mc *api.Machine, path string) error {
	role, db, err := roleDB(mc.Labels)
	if err != nil {
		return err
	}
	h, port, err := hostAddr(mc)
	if err != nil {
		return err
	}
	pw, err := dbstate.ReadPassword(mc.ID)
	if err != nil {
		return err
	}
	hp := net.JoinHostPort(h, strconv.Itoa(port))
	u := url.URL{Scheme: "postgres", User: url.UserPassword(role, pw), Host: hp, Path: "/" + db, RawQuery: "sslmode=disable"}
	body := fmt.Sprintf("# kling db branch -switch: database of this branch (copy %s). Secret: do not copy it.\n"+
		"DATABASE_URL=%s\nPGHOST=%s\nPGPORT=%d\nPGUSER=%s\nPGDATABASE=%s\nPGPASSWORD=%s\nPGSSLMODE=disable\n",
		mc.Name, u.String(), h, port, role, db, envQuote(pw))
	if engineOf(mc.Labels) == engineMySQL {
		// Las variables que leen los clientes de MySQL/MariaDB (MYSQL_PWD) y
		// las habituales de las aplicaciones.
		u.Scheme, u.RawQuery = "mysql", ""
		body = fmt.Sprintf("# kling db branch -switch: database of this branch (copy %s). Secret: do not copy it.\n"+
			"DATABASE_URL=%s\nMYSQL_HOST=%s\nMYSQL_TCP_PORT=%d\nMYSQL_USER=%s\nMYSQL_DATABASE=%s\nMYSQL_PWD=%s\n",
			mc.Name, envQuote(u.String()), h, port, role, db, envQuote(pw))
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".kling-db.env.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op tras el rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ── -ls, -rm, -prune ─────────────────────────────────────────────────────────

// branchRow es una fila de -ls.
type branchRow struct {
	Branch   string `json:"branch"`
	Key      string `json:"key"`
	Copy     string `json:"copy"`
	State    string `json:"state"`
	Ready    bool   `json:"ready"`
	Bytes    int64  `json:"disk_bytes"`
	LastUsed string `json:"last_used,omitempty"`
	Current  bool   `json:"current,omitempty"`
	Gone     bool   `json:"gone,omitempty"` // la rama ya no existe en git
}

func (a *app) branchLs(ctx context.Context, owner string, asJSON bool) error {
	ri, err := a.repo(ctx)
	if err != nil {
		return err
	}
	copies, err := a.repoCopies(ctx, ri, owner)
	if err != nil {
		return err
	}
	branches, err := a.localBranches(ctx)
	if err != nil {
		return err
	}
	cur, _ := a.currentBranch(ctx)
	rows := []branchRow{}
	for k, l := range copies {
		for _, mc := range l {
			name, ok := branches[k]
			if !ok {
				name = k
			}
			r := branchRow{Branch: name, Key: k, Copy: mc.Name, State: string(mc.State),
				Ready: mc.Labels[labelState] == stateReady, Bytes: mc.DiskBytes, Gone: !ok,
				Current: cur != "" && k == branchKey(cur)}
			if n, err := strconv.ParseInt(mc.Labels[labelUsed], 10, 64); err == nil && n > 0 {
				r.LastUsed = time.Unix(n, 0).UTC().Format(time.RFC3339)
			}
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Branch < rows[j].Branch })
	if asJSON {
		return json.NewEncoder(a.stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.stdout, "no branch of this repository has a copy (kling db branch -golden <template>)")
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "BRANCH\tCOPY\tSTATE\tSIZE\tLAST USED")
	for _, r := range rows {
		name := r.Branch
		if r.Current {
			name = "* " + name
		}
		if r.Gone {
			name += " (gone from git)"
		}
		st := r.State
		if !r.Ready {
			st += " (not ready)"
		}
		used := "-"
		if r.LastUsed != "" {
			used = r.LastUsed
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, r.Copy, st, sizeCol(r.Bytes), used)
	}
	return tw.Flush()
}

func sizeCol(n int64) string {
	if n <= 0 {
		return "-"
	}
	return humanBytes(n)
}

// removeKey borra la copia de una clave de rama de este repo y dueño.
func (a *app) removeKey(ctx context.Context, copies map[string][]*api.Machine, key, owner string) (bool, error) {
	l := copies[key]
	for _, mc := range l {
		if err := owned(mc, owner); err != nil {
			return false, err
		}
		if err := a.remove(ctx, mc); err != nil {
			return false, err
		}
		fmt.Fprintln(a.stdout, mc.Name)
	}
	return len(l) > 0, nil
}

func (a *app) branchRm(ctx context.Context, branch, owner string) error {
	if err := validBranch(branch); err != nil {
		return err
	}
	ri, err := a.repo(ctx)
	if err != nil {
		return err
	}
	un, err := a.lockRepo(ctx, ri)
	if err != nil {
		return err
	}
	defer un()
	copies, err := a.repoCopies(ctx, ri, owner)
	if err != nil {
		return err
	}
	key := branchKey(branch)
	ok, err := a.removeKey(ctx, copies, key, owner)
	if err != nil {
		return err
	}
	// El entorno de la rama (kling db branch -env), si lo hay, se va con ella.
	envGone := false
	for _, n := range branchEnvNames(ri, branch) {
		gone, err := a.envDown(ctx, n, owner)
		if err != nil {
			return err
		}
		if gone {
			envGone = true
			fmt.Fprintf(a.stdout, "environment %s removed\n", n)
		}
	}
	if !ok && !envGone {
		return fmt.Errorf("branch %s has no copy in this repository", branch)
	}
	// Su conexión ya no vale: fuera de .git también.
	if cur, err := a.currentBranch(ctx); err == nil && branchKey(cur) == key {
		_ = os.Remove(filepath.Join(ri.gitDir, branchEnvFile))
	}
	return nil
}

func (a *app) branchPrune(ctx context.Context, owner string, dry bool) error {
	ri, err := a.repo(ctx)
	if err != nil {
		return err
	}
	branches, err := a.localBranches(ctx)
	if err != nil {
		return err
	}
	// Sin ninguna rama (repo vacío o git raro) no se borra nada: podría ser un
	// fallo de lectura, y las copias no se recuperan.
	if len(branches) == 0 {
		return errors.New("git reports no local branches: refusing to prune")
	}
	if !dry {
		un, err := a.lockRepo(ctx, ri)
		if err != nil {
			return err
		}
		defer un()
	}
	copies, err := a.repoCopies(ctx, ri, owner)
	if err != nil {
		return err
	}
	var keys []string
	for k := range copies {
		if _, ok := branches[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Fprintln(a.stderr, "nothing to prune")
		return nil
	}
	for _, k := range keys {
		if dry {
			for _, mc := range copies[k] {
				fmt.Fprintf(a.stdout, "would remove %s (branch key %s)\n", mc.Name, k)
			}
			continue
		}
		if _, err := a.removeKey(ctx, copies, k, owner); err != nil {
			return err
		}
	}
	return nil
}
