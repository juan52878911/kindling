package main

// kling graph: grafos de microVMs (docs/grafos.md). Un fichero describe los
// nodos y sus aristas; el daemon arranca el grafo, resuelve cada arista en
// cada conexión y lo congela, guarda, ramifica o borra entero.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const graphUsage = "usage: kling graph <up|ls|inspect|freeze|thaw|snapshot|fork|rm> [...]"

func cmdGraph(args []string) error {
	if len(args) == 0 {
		return &errWithHint{err: errors.New(graphUsage), hint: "kling help graph"}
	}
	switch args[0] {
	case "up":
		return graphUp(args[1:])
	case "ls", "list":
		return graphList(args[1:])
	case "inspect":
		return graphInspect(args[1:])
	case "freeze", "thaw":
		return graphLifecycle(args[0], args[1:])
	case "snapshot":
		return graphSnapshot(args[1:])
	case "fork":
		return graphFork(args[1:])
	case "rm", "remove":
		return graphRemove(args[1:])
	}
	return fmt.Errorf("unknown subcommand %q: use up, ls, inspect, freeze, thaw, snapshot, fork or rm", args[0])
}

// fileGraph es el fichero de un grafo: el api.Graph de siempre y, en cada
// arista credential, de dónde sale su clave (nunca la clave en el fichero).
type fileGraph struct {
	Name  string                   `json:"name"`
	Nodes map[string]api.GraphNode `json:"nodes"`
	Edges []fileEdge               `json:"edges,omitempty"`
}

type fileEdge struct {
	api.GraphEdge
	// SecretEnv es una variable de entorno del CLI con la clave.
	SecretEnv string `json:"secret_env,omitempty"`
	// SecretFile es un fichero con la clave (relativo al del grafo).
	SecretFile string `json:"secret_file,omitempty"`
}

// esYAML decide el formato: por extensión, y si no la hay, JSON si empieza
// por '{'.
func esYAML(path string, datos []byte) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return true
	case ".json":
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(string(datos)), "{")
}

// leerGrafo interpreta el fichero (JSON o el subconjunto de YAML) en una
// petición de up. leerClave lee la de una arista (variable, fichero o stdin).
func leerGrafo(path string, datos []byte, leerClave func(e fileEdge, unica bool) (string, error)) (api.GraphRequest, error) {
	if esYAML(path, datos) {
		v, err := parseYAML(datos)
		if err != nil {
			return api.GraphRequest{}, fmt.Errorf("%s: %w", path, err)
		}
		if datos, err = json.Marshal(v); err != nil {
			return api.GraphRequest{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	var fg fileGraph
	dec := json.NewDecoder(bytes.NewReader(datos))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fg); err != nil {
		return api.GraphRequest{}, fmt.Errorf("%s: %w", path, err)
	}
	req := api.GraphRequest{Graph: api.Graph{Name: fg.Name, Nodes: fg.Nodes}}
	sinFuente := 0
	for _, e := range fg.Edges {
		if e.Kind == api.GraphEdgeCredential && e.SecretEnv == "" && e.SecretFile == "" {
			sinFuente++
		}
	}
	for _, e := range fg.Edges {
		if e.Kind != api.GraphEdgeCredential {
			if e.SecretEnv != "" || e.SecretFile != "" {
				return api.GraphRequest{}, fmt.Errorf("%s: edge %s -> %s: secret_env and secret_file are only for credential edges", path, e.From, e.To)
			}
			req.Graph.Edges = append(req.Graph.Edges, e.GraphEdge)
			continue
		}
		clave, err := leerClave(e, sinFuente == 1)
		if err != nil {
			return api.GraphRequest{}, fmt.Errorf("%s: edge %s -> %s (%s): %w", path, e.From, e.To, e.Env, err)
		}
		if req.Secrets == nil {
			req.Secrets = map[string]string{}
		}
		req.Secrets[e.Key()] = clave
		req.Graph.Edges = append(req.Graph.Edges, e.GraphEdge)
	}
	// Se valida aquí también: el error sale antes de hablar con el daemon,
	// con el nombre del fichero delante.
	g := req.Graph
	if err := api.ValidateGraph(&g); err != nil {
		return api.GraphRequest{}, fmt.Errorf("%s: %w", path, err)
	}
	return req, nil
}

// leerClaveDe lee la clave de una arista: de secret_env, de secret_file o,
// si es la única sin fuente y stdin no es una terminal, de stdin.
func leerClaveDe(dir string, stdin io.Reader, stdinTTY bool) func(e fileEdge, unica bool) (string, error) {
	return func(e fileEdge, unica bool) (string, error) {
		var clave string
		switch {
		case e.SecretEnv != "" && e.SecretFile != "":
			return "", errors.New("secret_env and secret_file exclude each other")
		case e.SecretEnv != "":
			clave = os.Getenv(e.SecretEnv)
			if clave == "" {
				return "", fmt.Errorf("environment variable %s is empty", e.SecretEnv)
			}
		case e.SecretFile != "":
			p := e.SecretFile
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			clave = strings.TrimRight(string(b), "\r\n")
		case unica && !stdinTTY:
			b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
			if err != nil {
				return "", err
			}
			clave = strings.TrimRight(string(b), "\r\n")
		default:
			return "", errors.New("needs its key: secret_env (a variable) or secret_file (a file); " +
				"with a single credential edge, stdin also works")
		}
		if clave == "" {
			return "", errors.New("the key is empty")
		}
		return clave, nil
	}
}

// errSinGrafos traduce el 404 de un daemon sin la capacidad.
func errSinGrafos(err error) error {
	var se *api.StatusError
	if errors.As(err, &se) && se.Code == 404 && se.Message == "404 Not Found" {
		return &errWithHint{err: errors.New("this daemon doesn't know graphs"),
			hint: "upgrade kindling on the host (the daemon needs the \"graphs\" capability)"}
	}
	return err
}

func graphUp(args []string) error {
	fs := flag.NewFlagSet("graph up", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only the graph id")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 1 {
		return errors.New("usage: kling graph up <file.json|file.yaml> [-json] [-q]")
	}
	path := fs.Arg(0)
	datos, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	req, err := leerGrafo(path, datos, leerClaveDe(filepath.Dir(path), os.Stdin, isTerminal(os.Stdin.Fd())))
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	start := time.Now()
	g, err := api.NewClient(hostOf(*host)).GraphUp(ctx, req)
	if err != nil {
		return errSinGrafos(err)
	}
	switch {
	case *asJSON:
		return json.NewEncoder(os.Stdout).Encode(g)
	case *quiet:
		fmt.Println(shortID(g.ID))
		return nil
	}
	fmt.Printf("graph %s (%s) up in %s\n", g.Name, shortID(g.ID), time.Since(start).Round(time.Millisecond))
	escribirNodos(os.Stdout, g)
	next("kling graph inspect %s   ·   kling graph freeze %s   ·   kling graph rm %s", g.Name, g.Name, g.Name)
	return nil
}

// escribirNodos imprime la tabla de nodos de un grafo.
func escribirNodos(w io.Writer, g *api.Graph) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NODE\tSTATE\tMACHINE\tWAKE\tFROM\tPORTS")
	for _, n := range g.SortedNodeNames() {
		nd := g.Nodes[n]
		estado := nd.State
		if nd.MachineID == "" {
			estado = "(not started)"
		}
		maq := "-"
		if nd.MachineID != "" {
			maq = shortID(nd.MachineID)
		}
		origen := nd.From
		if origen == "" {
			origen = "image " + nd.Image
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", n, estado, maq, nd.Wake, origen, puertosTexto(nd.Ports))
	}
	_ = tw.Flush()
}

func puertosTexto(ps []int) string {
	if len(ps) == 0 {
		return "-"
	}
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ",")
}

func graphList(args []string) error {
	fs := flag.NewFlagSet("graph ls", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only the names")
	_ = fs.Parse(reorderFor(fs, args))
	ctx, stop := ctxWithSignals()
	defer stop()
	gs, err := api.NewClient(hostOf(*host)).Graphs(ctx)
	if err != nil {
		return errSinGrafos(err)
	}
	switch {
	case *asJSON:
		if gs == nil {
			gs = []*api.Graph{}
		}
		return json.NewEncoder(os.Stdout).Encode(gs)
	case *quiet:
		for _, g := range gs {
			fmt.Println(g.Name)
		}
		return nil
	}
	if len(gs) == 0 {
		fmt.Println("no graphs")
		next("kling graph up <file.yaml>")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tSTATE\tNODES\tEDGES\tGEN\tFORK OF")
	for _, g := range gs {
		forkOf := "-"
		if g.ForkOf != "" {
			forkOf = shortID(g.ForkOf)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", g.Name, shortID(g.ID), g.State, len(g.Nodes), len(g.Edges), g.Generation, forkOf)
	}
	return tw.Flush()
}

func graphInspect(args []string) error {
	fs := flag.NewFlagSet("graph inspect", flag.ExitOnError)
	host := hostFlag(fs)
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 1 {
		return errors.New("usage: kling graph inspect <graph> [-json]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	g, err := api.NewClient(hostOf(*host)).Graph(ctx, fs.Arg(0))
	if err != nil {
		return errSinGrafos(err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(g)
	}
	escribirGrafo(os.Stdout, g)
	return nil
}

// escribirGrafo es la vista de inspect: cabecera, nodos y aristas.
func escribirGrafo(w io.Writer, g *api.Graph) {
	fmt.Fprintf(w, "graph %s (%s)  %s  generation %d\n", g.Name, g.ID, g.State, g.Generation)
	if g.ForkOf != "" {
		fmt.Fprintf(w, "fork of %s\n", g.ForkOf)
	}
	fmt.Fprintln(w, "\nnodes")
	escribirNodos(w, g)
	if len(g.Edges) == 0 {
		return
	}
	fmt.Fprintln(w, "\nedges")
	aristas := append([]api.GraphEdge(nil), g.Edges...)
	sort.SliceStable(aristas, func(i, j int) bool { return aristas[i].From < aristas[j].From })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range aristas {
		destino, extra := fmt.Sprintf("%s:%d", e.Host(), e.Port), ""
		switch e.Kind {
		case api.GraphEdgeCredential:
			extra = fmt.Sprintf("%s as %s on %s", e.Env, e.User, e.Database)
		case api.GraphEdgeShare:
			destino, extra = e.To, fmt.Sprintf("%s (%s)", e.Mount, e.Mode)
		case api.GraphEdgeDepends:
			destino, extra = e.To, "waits until it runs"
			if e.Port != 0 {
				extra = fmt.Sprintf("waits until port %d answers", e.Port)
			}
		}
		fmt.Fprintf(tw, "  %s -> %s\t%s\t%s\n", e.From, destino, e.Kind, extra)
	}
	_ = tw.Flush()
}

func graphLifecycle(verbo string, args []string) error {
	fs := flag.NewFlagSet("graph "+verbo, flag.ExitOnError)
	host := hostFlag(fs)
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kling graph %s <graph>", verbo)
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	start := time.Now()
	var g *api.Graph
	var err error
	if verbo == "freeze" {
		g, err = c.GraphFreeze(ctx, fs.Arg(0))
	} else {
		g, err = c.GraphThaw(ctx, fs.Arg(0))
	}
	if g != nil && g.Name != "" {
		fmt.Printf("graph %s: %s in %s\n", g.Name, g.State, time.Since(start).Round(time.Millisecond))
		escribirNodos(os.Stdout, g)
	}
	if err != nil {
		return errSinGrafos(err)
	}
	return nil
}

func graphSnapshot(args []string) error {
	fs := flag.NewFlagSet("graph snapshot", flag.ExitOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "prefix of the templates (default: the graph's name)")
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 1 {
		return errors.New("usage: kling graph snapshot <graph> [-name N] [-json]")
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	start := time.Now()
	s, err := api.NewClient(hostOf(*host)).GraphSnapshot(ctx, fs.Arg(0), api.GraphSnapshotRequest{Name: *name})
	if err != nil {
		return errSinGrafos(err)
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(s)
	}
	fmt.Printf("graph %s: generation %d saved in %s (one instant for every node)\n", s.Graph, s.Generation, time.Since(start).Round(time.Millisecond))
	nodos := make([]string, 0, len(s.Templates))
	for n := range s.Templates {
		nodos = append(nodos, n)
	}
	sort.Strings(nodos)
	for _, n := range nodos {
		fmt.Printf("  %s  ->  template %s\n", n, s.Templates[n])
	}
	next("use them as from: in a graph file, or kling run -from <template>")
	return nil
}

func graphFork(args []string) error {
	fs := flag.NewFlagSet("graph fork", flag.ExitOnError)
	host := hostFlag(fs)
	n := fs.Int("n", 1, fmt.Sprintf("how many copies (1-%d)", api.GraphForkMax))
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "print only the names of the new graphs")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 1 {
		return errors.New("usage: kling graph fork <graph> [-n N] [-q] [-json]")
	}
	if *n < 1 || *n > api.GraphForkMax {
		return fmt.Errorf("-n must be between 1 and %d", api.GraphForkMax)
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	start := time.Now()
	res, err := api.NewClient(hostOf(*host)).GraphFork(ctx, fs.Arg(0), api.GraphForkRequest{Count: *n})
	if err != nil {
		return errSinGrafos(err)
	}
	switch {
	case *asJSON:
		if res.Graphs == nil {
			res.Graphs = []*api.Graph{}
		}
		return json.NewEncoder(os.Stdout).Encode(res)
	case *quiet:
		for _, g := range res.Graphs {
			fmt.Println(g.Name)
		}
		return nil
	}
	fmt.Printf("%s branched into %d graph(s) in %s\n", fs.Arg(0), len(res.Graphs), time.Since(start).Round(time.Millisecond))
	for _, g := range res.Graphs {
		fmt.Printf("  %s  %s  %s\n", shortID(g.ID), g.Name, g.State)
	}
	if len(res.Graphs) > 0 {
		next("kling graph inspect %s   ·   kling graph rm %s", res.Graphs[0].Name, res.Graphs[0].Name)
	}
	return nil
}

func graphRemove(args []string) error {
	fs := flag.NewFlagSet("graph rm", flag.ExitOnError)
	host := hostFlag(fs)
	force := fs.Bool("f", false, "don't ask when removing several")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() == 0 {
		return errors.New("usage: kling graph rm [-f] <graph>...")
	}
	if !*force && !confirmMany("graph", fs.Args()) {
		return errAborted
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	var fallos int
	for _, ref := range fs.Args() {
		if err := c.GraphRemove(ctx, ref); err != nil {
			printError(os.Stderr, fmt.Errorf("%s: %w", ref, errSinGrafos(err)))
			fallos++
			continue
		}
		fmt.Printf("graph %s removed (its machines too)\n", ref)
	}
	if fallos > 0 {
		return &errConCodigo{code: 1, err: fmt.Errorf("%d graph(s) could not be removed", fallos)}
	}
	return nil
}
