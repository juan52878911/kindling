package main

// kling db env: un entorno de integración entero (app + copia de base) como un
// grafo del núcleo (docs/grafos.md).
//
// El grafo tiene dos nodos: db (una copia del golden, con su rol y su base) y
// app (la plantilla de la aplicación), unidos por UNA arista credential: es el
// `kling db attach` de siempre, declarado. La app solo ve un marcador en
// PGPASSWORD; el proxy de su máquina entra en `db.graph:5432` con la clave real.
//
// Seguridad. La clave se genera aquí y sigue solo en el host: al daemon llega
// por stdin de `kling graph up` (nunca en argv, en el entorno ni en el fichero
// del grafo, que no lleva secretos) y al invitado de db solo su verificador
// SCRAM, por stdin, como en rotate. Si algo falla, el grafo y la clave se
// deshacen. Solo se borra un entorno cuya copia de base es del dueño indicado.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	envNodeApp = "app"
	envNodeDB  = "db"
	// envGraphVar es la variable del marcador en la app.
	envGraphVar = defaultAttachEnv
	envUsage    = "usage: kling db env up <app-template> -golden G [-name N] [-allow d1,d2] [-app-port P] | down <name>"
)

// envNamePattern es el nombre de un grafo (el del núcleo: etiqueta DNS corta).
var envNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)

// envOpts son las opciones de env up.
type envOpts struct {
	name    string
	appTpl  string
	golden  string
	owner   string
	allow   []string // dominios de salida de la app (egress allowlist)
	appPort int      // puerto que la app expone (0 = ninguno)
}

func envGraphName(n string) error {
	if !envNamePattern.MatchString(n) {
		return fmt.Errorf("invalid environment name %q: lowercase letters, digits and '-', up to 24", n)
	}
	return nil
}

// envSpec arma el grafo: nada de secretos.
func envSpec(o envOpts, role, db string) api.Graph {
	app := api.GraphNode{From: o.appTpl, Egress: "allowlist", AllowDomains: o.allow}
	if o.appPort > 0 {
		app.Ports = []int{o.appPort}
	}
	return api.Graph{
		Name: o.name,
		Nodes: map[string]api.GraphNode{
			envNodeDB: {From: o.golden, Ports: []int{pgPort}, Labels: map[string]string{
				labelState: statePreparing, labelOwner: o.owner, labelGolden: o.golden,
				labelRole: role, labelDatabase: db,
			}},
			envNodeApp: app,
		},
		Edges: []api.GraphEdge{{From: envNodeApp, To: envNodeDB, Kind: api.GraphEdgeCredential,
			Port: pgPort, Env: envGraphVar, User: role, Database: db}},
	}
}

// graphFile escribe el grafo en un fichero 0600 de un directorio privado y
// devuelve su ruta y cómo borrarlo.
func graphFile(g api.Graph) (string, func(), error) {
	b, err := json.Marshal(struct {
		Name  string                   `json:"name"`
		Nodes map[string]api.GraphNode `json:"nodes"`
		Edges []api.GraphEdge          `json:"edges"`
	}{g.Name, g.Nodes, g.Edges})
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "kling-db-env-*")
	if err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, "graph.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return p, func() { os.RemoveAll(dir) }, nil
}

// graphOf lee un grafo por nombre; nil si no existe. "No existe" sale de la
// lista de grafos (`kling graph ls -json`), no del texto de un error: un
// fallo cualquiera cuyo mensaje dijera "404" o "not found" (el daemon caído,
// un proxy delante) no puede pasar por "no hay entorno" y hacer que se cree
// otro o que down diga que no había nada.
func (a *app) graphOf(ctx context.Context, name string) (*api.Graph, error) {
	out, err := a.k.Run(ctx, nil, "graph", "ls", "-json")
	if err != nil {
		return nil, err
	}
	if len(out) > maxJSON {
		return nil, errors.New("kling graph ls: output too large")
	}
	var gs []*api.Graph
	if err := json.Unmarshal(out, &gs); err != nil {
		return nil, fmt.Errorf("kling graph ls: %w", err)
	}
	for _, g := range gs {
		if g != nil && g.Name == name {
			return g, nil
		}
	}
	return nil, nil
}

// envUp crea el entorno y deja lista la base. Devuelve el grafo.
func (a *app) envUp(ctx context.Context, o envOpts) (*api.Graph, error) {
	if err := envGraphName(o.name); err != nil {
		return nil, err
	}
	if err := validOwner(o.owner); err != nil {
		return nil, err
	}
	for _, t := range []string{o.appTpl, o.golden} {
		if !namePattern.MatchString(t) {
			return nil, fmt.Errorf("invalid template name %q", t)
		}
	}
	snap, err := a.template(ctx, o.golden)
	if err != nil {
		return nil, err
	}
	role, db, err := goldenRoleDB(snap)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", o.golden, err)
	}
	pw, err := generatePassword()
	if err != nil {
		return nil, err
	}
	ver, err := newVerifier(pw)
	if err != nil {
		return nil, err
	}
	spec := envSpec(o, role, db)
	if err := api.ValidateGraph(&spec); err != nil {
		return nil, err
	}
	path, cleanup, err := graphFile(spec)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	fmt.Fprintf(a.stderr, "creating environment %s (%s + %s)...\n", o.name, o.appTpl, o.golden)
	// La clave por stdin: kling graph up la toma para la única arista credential.
	out, err := a.k.Run(ctx, strings.NewReader(pw+"\n"), "graph", "up", path, "-json")
	if err != nil {
		return nil, err
	}
	var g api.Graph
	if err := json.Unmarshal(out, &g); err != nil || g.Name == "" {
		a.envRollback(o.name, "")
		return nil, errors.New("kling graph up: unreadable answer")
	}
	dbID := g.Nodes[envNodeDB].MachineID
	if dbID == "" {
		a.envRollback(o.name, "")
		return nil, errors.New("the graph came up without a database machine")
	}
	if err := a.envPrepareDB(ctx, dbID, role, db, pw, ver); err != nil {
		a.envRollback(o.name, dbID)
		return nil, err
	}
	return &g, nil
}

// envPrepareDB deja lista la base del entorno: espera a Postgres, quita los
// roles heredados y estrena la clave (la misma que el daemon guarda para la
// arista). La clave se escribe en el host antes de tocar la base.
func (a *app) envPrepareDB(ctx context.Context, id, role, db, pw, ver string) error {
	if err := a.waitPostgres(ctx, id); err != nil {
		return err
	}
	if err := a.purgeInheritedRoles(ctx, id, db); err != nil {
		return err
	}
	if err := dbstate.WritePassword(id, pw); err != nil {
		return fmt.Errorf("storing the password of %s: %w", shortID(id), err)
	}
	if err := a.setVerifier(ctx, id, role, ver); err != nil {
		return err
	}
	return a.setState(ctx, id, stateReady)
}

// envRollback deshace un entorno a medias. Contexto propio: Ctrl-C no lo impide.
func (a *app) envRollback(name, dbID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := a.k.Run(ctx, nil, "graph", "rm", name); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not remove graph %s: %v (kling graph rm %s)\n", name, err, name)
	}
	if dbID != "" {
		if err := dbstate.Remove(dbID); err != nil {
			fmt.Fprintf(a.stderr, "warning: could not remove the password of %s: %v\n", shortID(dbID), err)
		}
	}
}

// envDown borra el entorno y la clave de su base. Devuelve false si no existía.
func (a *app) envDown(ctx context.Context, name, owner string) (bool, error) {
	if err := envGraphName(name); err != nil {
		return false, err
	}
	if err := validOwner(owner); err != nil {
		return false, err
	}
	g, err := a.graphOf(ctx, name)
	if err != nil || g == nil {
		return false, err
	}
	dbID := g.Nodes[envNodeDB].MachineID
	if dbID == "" {
		return false, fmt.Errorf("graph %s is not a kling db environment (no %s node with a machine)", name, envNodeDB)
	}
	cp, err := a.inspect(ctx, dbID)
	if err != nil {
		return false, err
	}
	// Solo se borra un entorno de este dueño: el de la copia de base.
	if cp.Labels[labelGolden] == "" || cp.Labels[labelOwner] != owner {
		return false, fmt.Errorf("graph %s is not a kling db environment of owner %q", name, owner)
	}
	if _, err := a.k.Run(ctx, nil, "graph", "rm", name); err != nil {
		return false, err
	}
	if err := dbstate.Remove(dbID); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not remove the password of %s: %v\n", shortID(dbID), err)
	}
	return true, nil
}

func parseAllow(s string) []string {
	var out []string
	for _, d := range strings.Split(s, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func cmdEnv(args []string) error {
	if len(args) == 0 {
		return usageErr("%s", envUsage)
	}
	sub, rest := args[0], args[1:]
	fs, host, owner := newFlags("env " + sub)
	name := fs.String("name", "", "up: name of the environment (default: env-<random>)")
	golden := fs.String("golden", "", "up: template of the database")
	allow := fs.String("allow", "", "up: comma-separated domains the app may reach (egress allowlist)")
	port := fs.Int("app-port", 0, "up: port the app exposes")
	pos, err := parse(fs, rest)
	if err != nil {
		return err
	}
	switch {
	case sub == "up" && len(pos) == 1 && *golden != "":
	case sub == "down" && len(pos) == 1 && *name == "" && *golden == "" && *allow == "" && *port == 0:
	default:
		return usageErr("%s", envUsage)
	}
	if *port < 0 || *port > 65535 || *port == 8080 {
		return usageErr("-app-port: 1-65535, and not 8080 (the guest agent's)")
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	if sub == "down" {
		ok, err := a.envDown(ctx, pos[0], *owner)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no environment %q", pos[0])
		}
		fmt.Fprintf(a.stdout, "environment %s removed\n", pos[0])
		return nil
	}
	n := *name
	if n == "" {
		n = "env-" + randomSuffix()
	}
	g, err := a.envUp(ctx, envOpts{name: n, appTpl: pos[0], golden: *golden, owner: *owner, allow: parseAllow(*allow), appPort: *port})
	if err != nil {
		return err
	}
	a.envReport(g)
	return nil
}

func (a *app) envReport(g *api.Graph) {
	fmt.Fprintf(a.stdout, "environment %s  up  (graph %s)\n", g.Name, shortID(g.ID))
	fmt.Fprintf(a.stdout, "  database:   %s-%s  (kling db connect %s-%s -psql)\n", g.Name, envNodeDB, g.Name, envNodeDB)
	fmt.Fprintf(a.stdout, "  in the app: host=%s.%s port=%d sslmode=disable, password = $%s (a placeholder; the real one stays on the host)\n",
		envNodeDB, api.GraphDomain, pgPort, envGraphVar)
	fmt.Fprintf(a.stdout, "  remove:     kling db env down %s\n", g.Name)
}
