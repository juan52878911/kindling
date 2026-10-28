package main

// `kling compose`: un compose.yaml de desarrollo, como `docker compose`.
// `up` traduce con from-compose (los bind mounts pasan a carpetas vivas rw),
// importa las imágenes en paralelo, levanta el grafo, pide al daemon que
// publique los `ports:` en 127.0.0.1 y junta la salida de los servicios.
// Ctrl-C quita el proyecto (-keep lo deja); `-d` lo deja levantado y vuelve.
//
// Los puertos los publica el DAEMON (internal/publicar), no este proceso:
// siguen ahí al cerrar el CLI (`up -d`) y tras reiniciar el daemon, y la
// primera conexión despierta una máquina congelada. Solo 127.0.0.1: Docker
// publica en todas las interfaces por defecto, aquí no se abre nada hacia la
// red. Contra un daemon sin api.CapabilityPublish, los publica este proceso
// mientras corre, como antes.
//
// Un proyecto es de UN compose: sus máquinas llevan kling.compose.file con la
// ruta del fichero, y un `up` o un `down` de otro fichero con el mismo nombre
// de proyecto se niega en vez de llevarse el ajeno. Dos `up` del mismo
// proyecto a la vez, tampoco (cerrojoProyecto).

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const composeUsage = `usage: kling compose [-f compose.yaml] [-p name] <command> [flags] [service...]
  up [-d] [-keep] [-no-logs] [-t] [-fresh]   create and start the project (-d: leave it up and return)
  down [-v]                                  remove its machines and ports (-v: and its named volumes)
  ps                                         list its services, their state and ports
  logs [-f] [-t] [-tail N] [service...]      the services' output (-f: follow)
  exec [-T] [-w DIR] [-e K=V] service cmd... run a command in a service (a terminal if there is one)
  stop|start|restart [service...]            freeze, wake up or restart services
Long flags work too (--detach, --volumes, --follow, --file, --project-name).`

// labelComposeFile es la ruta absoluta del compose de un proyecto, en cada
// una de sus máquinas.
const labelComposeFile = "kling.compose.file"

// globalesCompose son -f y -p delante del subcomando, como en Docker
// (`docker compose -f x.yaml -p demo logs -f`).
type globalesCompose struct{ file, name string }

func cmdCompose(args []string) error {
	var g globalesCompose
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		k, v, conValor := strings.Cut(strings.TrimLeft(args[0], "-"), "=")
		switch k {
		case "f", "file", "p", "project-name":
		default:
			return &errWithHint{err: fmt.Errorf("unknown flag %s before the command", args[0]), hint: composeUsage}
		}
		if !conValor {
			if len(args) < 2 {
				return fmt.Errorf("%s needs a value", args[0])
			}
			v, args = args[1], args[1:]
		}
		if k == "f" || k == "file" {
			g.file = v
		} else {
			g.name = v
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return &errWithHint{err: errors.New(composeUsage), hint: "kling help compose"}
	}
	switch args[0] {
	case "up":
		return composeUp(g, args[1:])
	case "down":
		return composeDown(g, args[1:])
	case "ps":
		return composePS(g, args[1:])
	case "logs":
		return composeLogs(g, args[1:])
	case "exec":
		return composeExec(g, args[1:])
	case "stop", "start", "restart":
		return composeCiclo(g, args[0], args[1:])
	}
	return &errWithHint{err: fmt.Errorf("unknown subcommand %q", args[0]), hint: composeUsage}
}

// flagsCompose son -f y -p de un subcomando (además de los de delante) y
// sus nombres largos; -H es el del daemon.
func flagsCompose(fs *flag.FlagSet, g globalesCompose, conFichero bool) (host, file, name *string) {
	host = hostFlag(fs)
	file, name = new(string), new(string)
	*file, *name = g.file, g.name
	if conFichero {
		fs.StringVar(file, "f", g.file, "compose file (default: compose.yaml, compose.yml, docker-compose.yaml, docker-compose.yml)")
	}
	fs.StringVar(file, "file", g.file, "compose file")
	fs.StringVar(name, "p", g.name, "project (graph) name (default: name: of the file, or the directory's)")
	fs.StringVar(name, "project-name", g.name, "project name")
	return host, file, name
}

// composeFiles son los nombres que busca `docker compose`, en su orden.
var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

func findCompose(f string) (string, error) {
	if f != "" {
		return f, nil
	}
	for _, n := range composeFiles {
		if _, err := os.Stat(n); err == nil {
			return n, nil
		}
	}
	return "", fmt.Errorf("no compose file here (%s); use -f", strings.Join(composeFiles, ", "))
}

// leerCompose lee y traduce el compose de -f/-p, con el .env de su
// directorio. Devuelve también su ruta absoluta.
func leerCompose(file, name string) (*traduccionCompose, string, error) {
	path, err := findCompose(file)
	if err != nil {
		return nil, "", err
	}
	datos, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	proyecto := name
	if proyecto == "" {
		proyecto = filepath.Base(filepath.Dir(abs))
	}
	lookup, err := conDotEnv(filepath.Dir(abs), os.LookupEnv)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", filepath.Dir(abs), err)
	}
	t, err := traducirComposeEn(datos, proyecto, name != "", lookup, filepath.Dir(abs))
	if err != nil {
		if t != nil {
			printAvisos(t.Avisos)
		}
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return t, abs, nil
}

// proyectoDe es el nombre del proyecto: -p, o el del compose.
func proyectoDe(file, name string) (proyecto, abs string, t *traduccionCompose, err error) {
	if name != "" && file == "" {
		if _, ferr := findCompose(""); ferr != nil {
			return nombreDNS(name), "", nil, nil
		}
	}
	t, abs, err = leerCompose(file, name)
	if err != nil {
		return "", "", nil, err
	}
	return t.Proyect, abs, t, nil
}

// printAvisos enseña los avisos de la traducción menos los que compose up
// resuelve: publicar puertos, el entorno (va en la máquina de la plantilla)
// y el healthcheck (se ejecuta dentro antes de guardarla). Los de claves sin
// equivalente llevan a la tabla de docs/docker-a-kindling.md.
func printAvisos(avisos []string) {
	sinSoporte := false
	for _, a := range avisos {
		if strings.Contains(a, "kling compose up does") || strings.Contains(a, "baked into the image") ||
			strings.Contains(a, "(the test itself is not run)") || strings.Contains(a, "healthcheck: nobody depends on it") ||
			strings.Contains(a, "ignored (the probe is a TCP connect") {
			continue
		}
		if strings.Contains(a, "not supported") || strings.Contains(a, "not carried") {
			sinSoporte = true
		}
		fmt.Fprintln(os.Stderr, a)
	}
	if sinSoporte {
		fmt.Fprintln(os.Stderr, "  what each unsupported key does here: "+docDockerAKindling)
	}
}

// docDockerAKindling es la tabla de equivalencias con Docker.
const docDockerAKindling = "https://github.com/juan52878911/kindling/blob/main/docs/docker-a-kindling.md"

// selfCmd es este mismo binario con el endpoint de -H en el entorno.
func selfCmd(ctx context.Context, host string, args ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	c := exec.CommandContext(ctx, self, args...)
	c.Env = os.Environ()
	if host != "" {
		c.Env = append(c.Env, "KLING_HOST="+host)
	}
	return c
}

// importCompose es la orden de import de un servicio. Sin -replace: import
// reconoce la misma receta y no hace nada (milisegundos); si la receta
// cambió, antesDeReplace (borrar las plantillas que salieron de la imagen
// vieja) y otra vez con -replace.
type importCompose struct {
	linea, conReplace string
	antesDeReplace    func()
	// yaReplace: directamente con -replace (compose up -fresh).
	yaReplace bool
}

// runSetup crea los volúmenes y después importa en paralelo.
func runSetup(ctx context.Context, host string, vols []string, imports []importCompose) error {
	run := func(l string) (string, error) {
		argv, err := shellWords(strings.TrimPrefix(l, "kling "))
		if err != nil {
			return "", err
		}
		out, err := selfCmd(ctx, host, argv...).CombinedOutput()
		s := strings.TrimSpace(string(out))
		if err != nil {
			if len(argv) >= 3 && argv[0] == "volume" && strings.Contains(s, "exists") {
				return s, nil // ya estaba: se reutiliza, como un volumen de Docker
			}
			return s, fmt.Errorf("%s: %s", l, s)
		}
		return s, nil
	}
	for _, l := range vols {
		if _, err := run(l); err != nil {
			return err
		}
	}
	errs := make([]error, len(imports))
	var wg sync.WaitGroup
	for i, im := range imports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if im.yaReplace {
				if im.antesDeReplace != nil {
					im.antesDeReplace()
				}
				_, errs[i] = run(im.conReplace)
				return
			}
			out, err := run(im.linea)
			if err != nil && strings.Contains(out, "-replace") {
				if im.antesDeReplace != nil {
					im.antesDeReplace()
				}
				_, err = run(im.conReplace)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// shellWords parte una línea de t.Antes (que sale de comillaSh) en palabras.
func shellWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, q := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q:
			if c == '\'' {
				q = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			q, in = true, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if q {
		return nil, fmt.Errorf("unbalanced quote in %q", s)
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

func composeUp(g globalesCompose, args []string) error {
	fs := flag.NewFlagSet("compose up", flag.ExitOnError)
	host, file, name := flagsCompose(fs, g, true)
	detach := fs.Bool("d", false, "leave the project up and return: the daemon keeps its ports published (kling compose down removes it)")
	fs.BoolVar(detach, "detach", false, "same as -d")
	keep := fs.Bool("keep", false, "leave the machines (and their ports) when this exits (kling compose down removes them)")
	noLogs := fs.Bool("no-logs", false, "do not follow the services' output")
	stamps := fs.Bool("t", false, "show the time of each line of the services' output (RFC 3339, UTC)")
	fs.BoolVar(stamps, "timestamps", false, "same as -t")
	fresh := fs.Bool("fresh", false, "rebuild the services' warm templates (after re-pulling an image tag, say)")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 0 {
		return errors.New(composeUsage)
	}
	t, abs, err := leerCompose(*file, *name)
	if err != nil {
		return err
	}
	printAvisos(t.Avisos)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	endpoint := hostOf(*host)
	c := api.NewClient(endpoint)
	t0 := time.Now()

	cerrojo, err := cerrojoProyecto(endpoint, t.Proyect)
	if err != nil {
		return err
	}
	defer cerrojo.Close()
	if err := comprobarDueno(ctx, c, t.Proyect, abs); err != nil {
		return err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	publicaDaemon := slices.Contains(info.Capabilities, api.CapabilityPublish)
	if *detach && !publicaDaemon {
		return fmt.Errorf("the daemon (%s) does not publish ports itself, so -d would leave them unpublished: update it, or run without -d", info.Version)
	}
	t.etiquetas = map[string]string{labelComposeProject: t.Proyect, labelComposeFile: abs}

	// Un up de un proyecto que ya está: se sustituye (sus volúmenes siguen).
	quitarProyecto(ctx, c, t.Proyect)
	quitarHuerfanas(ctx, c, t)
	// Un puerto ocupado se dice antes de importar ni preparar nada.
	if esLocal(endpoint) {
		if err := comprobarPuertosLibres(t); err != nil {
			return err
		}
	}
	if err := prepararCompose(ctx, c, *host, t, *fresh); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "kling-compose-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(t.Grafo); err != nil {
		return err
	}
	tmp.Close()
	up := selfCmd(ctx, *host, "graph", "up", "-q", tmp.Name())
	up.Stdin, up.Stderr = os.Stdin, os.Stderr
	if _, err := up.Output(); err != nil {
		return fmt.Errorf("graph up: %w", err)
	}

	var lns []net.Listener
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	if publicaDaemon {
		if err := publicarCompose(ctx, c, t); err != nil {
			quitarProyecto(context.WithoutCancel(ctx), c, t.Proyect)
			return err
		}
	} else {
		lns = publicarEnCLI(ctx, c, t)
	}
	fmt.Fprintf(os.Stderr, "%s is up in %.1fs\n", t.Proyect, time.Since(t0).Seconds())
	if *detach {
		fmt.Fprintf(os.Stderr, "the daemon keeps the ports published; kling compose logs -f follows it, kling compose down removes it\n")
		return nil
	}

	if !*noLogs {
		seguirServicios(ctx, *host, t.Proyect, nodosDe(t), *stamps)
	}
	switch {
	case *keep && publicaDaemon:
		fmt.Fprintln(os.Stderr, "Ctrl-C stops following; the machines and their ports stay (kling compose down removes them)")
	case *keep:
		fmt.Fprintln(os.Stderr, "Ctrl-C stops publishing and following; the machines stay (kling compose down removes them)")
	default:
		fmt.Fprintln(os.Stderr, "Ctrl-C removes the machines and their ports (named volumes stay)")
	}
	<-ctx.Done()
	if *keep {
		return nil
	}
	fmt.Fprintf(os.Stderr, "\nremoving %s...\n", t.Proyect)
	quitarProyecto(context.Background(), c, t.Proyect)
	return nil
}

// publicarCompose pide al daemon que publique los ports: de cada nodo. Un
// puerto ocupado es un error (el up se deshace); UDP en el Mac, un aviso.
func publicarCompose(ctx context.Context, c *api.Client, t *traduccionCompose) error {
	for _, nodo := range clavesOrd(t.Publica) {
		pub, err := c.Publish(ctx, t.Proyect+"-"+nodo, t.Publica[nodo])
		var se *api.StatusError
		if errors.As(err, &se) && se.Code == 501 {
			// Solo UDP: los TCP del nodo, sin él.
			var tcp []api.PortPublish
			for _, p := range t.Publica[nodo] {
				if p.ProtoOr() == api.ProtoTCP {
					tcp = append(tcp, p)
				}
			}
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", nodo, se.Message)
			if len(tcp) == 0 {
				continue
			}
			pub, err = c.Publish(ctx, t.Proyect+"-"+nodo, tcp)
		}
		if err != nil {
			return fmt.Errorf("%s: publishing its ports: %w", nodo, err)
		}
		for _, p := range pub.Ports {
			fmt.Fprintf(os.Stderr, "  %-12s %s\n", nodo, p.String())
		}
	}
	return nil
}

// esLocal dice si el daemon de endpoint corre en esta máquina (un socket
// Unix): entonces sus puertos publicados son los de aquí.
func esLocal(endpoint string) bool {
	return endpoint == "" || strings.HasPrefix(endpoint, "unix:") || strings.HasPrefix(endpoint, "/")
}

// comprobarPuertosLibres abre y cierra cada puerto del host de t: uno
// ocupado se dice antes de importar ni preparar plantillas.
func comprobarPuertosLibres(t *traduccionCompose) error {
	for _, nodo := range clavesOrd(t.Publica) {
		for _, p := range t.Publica[nodo] {
			if p.Host == 0 {
				continue
			}
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Host))
			if p.ProtoOr() == api.ProtoUDP {
				pc, err := net.ListenPacket("udp", addr)
				if err != nil {
					return fmt.Errorf("%s: ports: 127.0.0.1:%d/udp is in use (%w)", nodo, p.Host, err)
				}
				pc.Close()
				continue
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return &errWithHint{err: fmt.Errorf("%s: ports: 127.0.0.1:%d is in use (%w)", nodo, p.Host, err),
					hint: "lsof -nP -iTCP:" + strconv.Itoa(p.Host) + " -sTCP:LISTEN   (says who has it)"}
			}
			ln.Close()
		}
	}
	return nil
}

// cerrojoProyecto toma el cerrojo de un `up` del proyecto contra ese daemon:
// dos a la vez se pisarían (uno borra lo que el otro levanta).
func cerrojoProyecto(endpoint, proyecto string) (io.Closer, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "kindling", "compose")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	h := sha256.Sum256([]byte(endpoint + "\x00" + proyecto))
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(h[:8])+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another kling compose up of project %s is running against this daemon; wait for it or stop it", proyecto)
		}
		return nil, err
	}
	return f, nil
}

// comprobarDueno se niega a tocar un proyecto que es de otro compose (otro
// fichero con el mismo nombre de proyecto) o un grafo que no hizo compose.
// abs vacío acepta cualquier compose (down -p sin fichero).
func comprobarDueno(ctx context.Context, c *api.Client, proyecto, abs string) error {
	g, err := c.Graph(ctx, proyecto)
	var se *api.StatusError
	if errors.As(err, &se) && se.Code == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	ms, err := c.List(ctx)
	if err != nil {
		return err
	}
	return duenoDe(g, ms, proyecto, abs)
}

func duenoDe(g *api.Graph, ms []*api.Machine, proyecto, abs string) error {
	deCompose, maquinas := false, 0
	for _, m := range ms {
		if m.Labels[api.LabelGraph] != g.ID {
			continue
		}
		maquinas++
		if f := m.Labels[labelComposeFile]; f != "" {
			if abs != "" && f != abs {
				return &errWithHint{err: fmt.Errorf("project %s belongs to another compose file (%s)", proyecto, f),
					hint: "give this one another name with -p, or remove that one: kling compose -f " + comillaSh(f) + " down"}
			}
			deCompose = true
		} else if m.Labels[labelComposeProject] == proyecto {
			deCompose = true // de un kling anterior, sin la ruta
		}
	}
	if !deCompose && maquinas > 0 {
		return &errWithHint{err: fmt.Errorf("graph %s exists and was not made by kling compose", proyecto),
			hint: "use another project name (-p), or remove it: kling graph rm " + proyecto}
	}
	return nil
}

// quitarProyecto deja de publicar los puertos del proyecto y borra su grafo
// (sus máquinas; los volúmenes se quedan).
func quitarProyecto(ctx context.Context, c *api.Client, proyecto string) {
	g, err := c.Graph(ctx, proyecto)
	if err != nil {
		return
	}
	for _, n := range g.Nodes {
		if n.MachineID != "" {
			_ = c.Unpublish(ctx, n.MachineID)
		}
	}
	if err := c.GraphRemove(ctx, proyecto); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removing %s: %v\n", proyecto, err)
	}
}

// quitarHuerfanas borra las máquinas <proyecto>-<nodo> de un grafo que ya
// no existe y las de preparación de plantillas que dejó un up interrumpido:
// un up que falló a medias puede dejar una fallida que el grafo no se llevó
// (o una -prep a la que Ctrl-C pilló dentro de su Run), y con ella el
// siguiente up falla por el nombre ("machine name is taken") hasta un kling
// rm a mano. Solo las que llevan la etiqueta de grafo o la del proyecto: una
// máquina del usuario con ese nombre no se toca. Se llama después de quitar
// el grafo del proyecto: lo que queda con su etiqueta es huérfano.
func quitarHuerfanas(ctx context.Context, c *api.Client, t *traduccionCompose) {
	ms, err := c.List(ctx)
	if err != nil {
		return
	}
	nombres := map[string]bool{}
	for _, s := range t.vivos {
		nombres[t.Proyect+"-"+s.nodo] = true
	}
	for _, m := range ms {
		huerfana := nombres[m.Name] && m.Labels[api.LabelGraph] != ""
		if m.Labels[labelComposeProject] == t.Proyect && strings.HasSuffix(m.Name, "-prep") {
			huerfana = true
		}
		if huerfana {
			fmt.Fprintf(os.Stderr, "removing %s, left %s by an earlier up\n", m.Name, m.State)
			_ = c.Remove(ctx, m.ID)
		}
	}
}

func composeDown(g globalesCompose, args []string) error {
	fs := flag.NewFlagSet("compose down", flag.ExitOnError)
	host, file, name := flagsCompose(fs, g, true)
	vols := fs.Bool("v", false, "also remove the project's named volumes")
	fs.BoolVar(vols, "volumes", false, "same as -v")
	_ = fs.Parse(reorderFor(fs, args))
	proyecto, abs, t, err := proyectoDe(*file, *name)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	if err := comprobarDueno(ctx, c, proyecto, abs); err != nil {
		return err
	}
	if _, err := c.Graph(ctx, proyecto); err != nil {
		var se *api.StatusError
		if !errors.As(err, &se) || se.Code != 404 {
			return err
		}
		fmt.Printf("%s is not up\n", proyecto)
	} else {
		quitarProyecto(ctx, c, proyecto)
		fmt.Printf("%s removed (its machines and ports)\n", proyecto)
	}
	if *vols {
		if t == nil {
			return errors.New("-v needs the compose file (to know its volumes): run it where the compose is, or with -f")
		}
		for _, l := range t.Antes {
			v, ok := strings.CutPrefix(l, "kling volume create ")
			if !ok {
				continue
			}
			if err := c.RemoveVolume(ctx, v); err != nil {
				var se *api.StatusError
				if errors.As(err, &se) && se.Code == 404 {
					continue
				}
				return fmt.Errorf("volume %s: %w", v, err)
			}
			fmt.Printf("volume %s removed\n", v)
		}
	}
	return nil
}

// serviciosDe son los nodos del proyecto que piden args (nombres del
// compose o del nodo; ninguno: todos), con su máquina.
func serviciosDe(ctx context.Context, c *api.Client, proyecto string, args []string) ([]string, *api.Graph, error) {
	g, err := c.Graph(ctx, proyecto)
	if err != nil {
		var se *api.StatusError
		if errors.As(err, &se) && se.Code == 404 {
			return nil, nil, &errWithHint{err: fmt.Errorf("%s is not up", proyecto), hint: "kling compose up -d"}
		}
		return nil, nil, err
	}
	var nodos []string
	for n := range g.Nodes {
		nodos = append(nodos, n)
	}
	sort.Strings(nodos)
	if len(args) == 0 {
		return nodos, g, nil
	}
	var out []string
	for _, a := range args {
		n := nombreDNS(a)
		if _, ok := g.Nodes[n]; !ok {
			return nil, nil, fmt.Errorf("%s has no service %s (it has %s)", proyecto, a, strings.Join(nodos, ", "))
		}
		out = append(out, n)
	}
	return out, g, nil
}

func composePS(g globalesCompose, args []string) error {
	fs := flag.NewFlagSet("compose ps", flag.ExitOnError)
	host, file, name := flagsCompose(fs, g, true)
	_ = fs.Parse(reorderFor(fs, args))
	proyecto, _, _, err := proyectoDe(*file, *name)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	nodos, gr, err := serviciosDe(ctx, c, proyecto, fs.Args())
	if err != nil {
		return err
	}
	pubs, _ := c.Publications(ctx)
	puertos := map[string][]api.PortPublish{}
	for _, p := range pubs {
		puertos[p.Machine] = p.Ports
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tMACHINE\tSTATE\tPORTS")
	for _, n := range nodos {
		nd := gr.Nodes[n]
		maq, estado := "-", nd.State
		if nd.MachineID != "" {
			if m, err := c.Get(ctx, nd.MachineID); err == nil {
				maq, estado = m.Name, string(m.State)
			}
		}
		if estado == "" {
			estado = "not started (lazy)"
		}
		ps := api.FormatPorts(puertos[nd.MachineID])
		if ps == "" {
			ps = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, maq, estado, ps)
	}
	return tw.Flush()
}

func composeLogs(g globalesCompose, args []string) error {
	fs := flag.NewFlagSet("compose logs", flag.ExitOnError)
	host, file, name := flagsCompose(fs, g, false)
	follow := fs.Bool("f", false, "follow the output")
	fs.BoolVar(follow, "follow", false, "same as -f")
	stamps := fs.Bool("t", false, "show the time of each line (RFC 3339, UTC)")
	fs.BoolVar(stamps, "timestamps", false, "same as -t")
	tail := fs.Int("tail", 100, "lines of each service to show first (0: all)")
	_ = fs.Parse(reorderFor(fs, args))
	proyecto, _, _, err := proyectoDe(*file, *name)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	nodos, _, err := serviciosDe(ctx, c, proyecto, fs.Args())
	if err != nil {
		return err
	}
	if *follow {
		seguirServicios(ctx, *host, proyecto, nodos, *stamps)
		<-ctx.Done()
		return nil
	}
	w := anchoNodos(nodos)
	for _, n := range nodos {
		a := []string{"logs", "-service", "-tail", strconv.Itoa(*tail)}
		if *stamps {
			a = append(a, "-t")
		}
		out, err := selfCmd(ctx, *host, append(a, proyecto+"-"+n)...).CombinedOutput()
		if err != nil && len(out) == 0 {
			return fmt.Errorf("%s: %w", n, err)
		}
		pre := fmt.Sprintf("%-*s | ", w, n)
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			fmt.Println(pre + sc.Text())
		}
	}
	return nil
}

// composeExec corre un comando en un servicio: con una terminal delante,
// como `docker compose exec` (kling shell); si no, o con -T, kling exec, con
// la entrada estándar si no es una terminal.
func composeExec(g globalesCompose, args []string) error {
	fs := flag.NewFlagSet("compose exec", flag.ExitOnError)
	host, file, name := flagsCompose(fs, g, false)
	sinTTY := fs.Bool("T", false, "no terminal (kling exec): for scripts and pipes")
	fs.BoolVar(sinTTY, "no-tty", false, "same as -T")
	dir := fs.String("w", "", "working directory inside the service")
	fs.StringVar(dir, "workdir", "", "same as -w")
	var env multiFlag
	fs.Var(&env, "e", "KEY=value for the command (repeatable)")
	fs.Var(&env, "env", "same as -e")
	// Sin reordenar: lo que va tras el servicio es el comando, con sus flags.
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("usage: kling compose exec [-T] [-w DIR] [-e K=V] <service> <cmd> [args...]")
	}
	servicio, cmd := rest[0], rest[1:]
	if cmd[0] == "--" {
		cmd = cmd[1:]
	}
	proyecto, _, _, err := proyectoDe(*file, *name)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	nodos, _, err := serviciosDe(ctx, c, proyecto, []string{servicio})
	if err != nil {
		return err
	}
	sub := "exec"
	var a []string
	if !*sinTTY && isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd()) {
		sub = "shell"
	} else if !isTerminal(os.Stdin.Fd()) {
		a = append(a, "-i")
	}
	if *dir != "" {
		a = append(a, "-w", *dir)
	}
	for _, e := range env {
		a = append(a, "-e", e)
	}
	argv := append(append(append([]string{sub}, a...), proyecto+"-"+nodos[0], "--"), cmd...)
	x := selfCmd(context.Background(), *host, argv...)
	x.Stdin, x.Stdout, x.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := x.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &errConCodigo{code: ee.ExitCode(), err: errSilencioso}
		}
		return err
	}
	return nil
}

// errSilencioso: el código de salida lo dice todo (el comando ya escribió
// lo suyo).
var errSilencioso = errors.New("")

// multiFlag acumula un flag repetible.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// composeCiclo es stop, start y restart de los servicios. stop CONGELA la
// máquina (libera su RAM y CPU; start la despierta en milisegundos con su
// memoria, no reinicia el proceso como docker start) y restart la para y la
// arranca en frío sobre su disco, con el entorno del compose.
func composeCiclo(g globalesCompose, que string, args []string) error {
	fs := flag.NewFlagSet("compose "+que, flag.ExitOnError)
	host, file, name := flagsCompose(fs, g, true)
	_ = fs.Parse(reorderFor(fs, args))
	proyecto, _, t, err := proyectoDe(*file, *name)
	if err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	nodos, gr, err := serviciosDe(ctx, c, proyecto, fs.Args())
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range nodos {
		id := gr.Nodes[n].MachineID
		if id == "" {
			continue
		}
		t0 := time.Now()
		var err error
		switch que {
		case "stop":
			_, err = c.Freeze(ctx, id)
		case "start":
			_, err = c.Thaw(ctx, id)
		case "restart":
			var env []string
			if t != nil && t.por != nil {
				for _, s := range t.vivos {
					if s.nodo == n {
						for _, kv := range s.env {
							env = append(env, kv[0]+"="+kv[1])
						}
					}
				}
			}
			if _, err = c.Stop(ctx, id); err == nil {
				_, err = c.Start(ctx, id, env)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n, err))
			continue
		}
		fmt.Printf("%s %s in %d ms\n", n, map[string]string{"stop": "frozen", "start": "running", "restart": "restarted"}[que], time.Since(t0).Milliseconds())
	}
	return errors.Join(errs...)
}

// nodosDe son los nodos del grafo traducido, en orden.
func nodosDe(t *traduccionCompose) []string {
	var out []string
	for _, l := range strings.Split(t.Grafo, "\n") {
		if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.Contains(l, ": {") {
			out = append(out, strings.TrimSpace(l[:strings.Index(l, ":")]))
		}
	}
	sort.Strings(out)
	return out
}

func anchoNodos(nodos []string) int {
	w := 0
	for _, n := range nodos {
		w = max(w, len(n))
	}
	return w
}

// seguirServicios sigue la salida de los nodos, juntos y con el nombre
// delante, hasta que se cancela ctx.
func seguirServicios(ctx context.Context, host, proyecto string, nodos []string, stamps bool) {
	w := anchoNodos(nodos)
	var mu sync.Mutex
	for _, n := range nodos {
		go followServiceLogs(ctx, host, proyecto+"-"+n, fmt.Sprintf("%-*s | ", w, n), stamps, &mu)
	}
}

// followServiceLogs sigue la salida del servicio de una máquina con un prefijo por
// línea, y vuelve a engancharse si se corta (una descongelación, un reinicio).
func followServiceLogs(ctx context.Context, host, machine, prefix string, stamps bool, mu *sync.Mutex) {
	tail := "20"
	for ctx.Err() == nil {
		args := []string{"logs", "-service", "-f", "-tail", tail}
		if stamps {
			args = append(args, "-t")
		}
		cmd := selfCmd(ctx, host, append(args, machine)...)
		out, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			mu.Lock()
			fmt.Fprintln(os.Stdout, prefix+sc.Text())
			mu.Unlock()
		}
		_ = cmd.Wait()
		tail = "0"
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}
