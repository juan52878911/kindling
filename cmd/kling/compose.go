package main

// `kling compose up`: un compose.yaml de desarrollo, como `docker compose up`
// en primer plano. Traduce con from-compose (los bind mounts pasan a carpetas
// vivas rw), importa las imágenes en paralelo, levanta el grafo, publica los
// `ports:` en 127.0.0.1 y junta la salida de los servicios. Ctrl-C quita el
// grafo (-keep lo deja).
//
// Los puertos los publica este proceso, no el daemon: en el Mac cada máquina
// se alcanza por un puerto de loopback que cambia con cada arranque o
// descongelación (Machine.Forwards), así que se resuelve en cada conexión, y
// una máquina congelada se despierta con la primera. Solo 127.0.0.1: Docker
// publica en todas las interfaces por defecto, aquí no se abre nada hacia la
// red.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const composeUsage = `usage: kling compose up [-f compose.yaml] [-p name] [-keep] [-no-logs] [-t] [-fresh]
       kling compose down [-f compose.yaml] [-p name]`

func cmdCompose(args []string) error {
	if len(args) == 0 {
		return &errWithHint{err: errors.New(composeUsage), hint: "kling help compose"}
	}
	switch args[0] {
	case "up":
		return composeUp(args[1:])
	case "down":
		return composeDown(args[1:])
	}
	return fmt.Errorf("unknown subcommand %q: use up or down", args[0])
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

// leerCompose lee y traduce el compose de -f/-p.
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
	t, err := traducirComposeEn(datos, proyecto, name != "", os.LookupEnv, filepath.Dir(abs))
	if err != nil {
		if t != nil {
			printAvisos(t.Avisos)
		}
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return t, path, nil
}

// printAvisos enseña los avisos de la traducción menos los que compose up
// resuelve: publicar puertos, el entorno (va en la máquina de la plantilla)
// y el healthcheck (se ejecuta dentro antes de guardarla).
func printAvisos(avisos []string) {
	for _, a := range avisos {
		if strings.Contains(a, "kling compose up does") || strings.Contains(a, "baked into the image") ||
			strings.Contains(a, "(the test itself is not run)") || strings.Contains(a, "healthcheck: nobody depends on it") ||
			strings.Contains(a, "ignored (the probe is a TCP connect") {
			continue
		}
		fmt.Fprintln(os.Stderr, a)
	}
}

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

func composeUp(args []string) error {
	fs := flag.NewFlagSet("compose up", flag.ExitOnError)
	host := hostFlag(fs)
	file := fs.String("f", "", "compose file (default: compose.yaml, compose.yml, docker-compose.yaml, docker-compose.yml)")
	name := fs.String("p", "", "project (graph) name (default: name: of the file, or the directory's)")
	keep := fs.Bool("keep", false, "leave the machines running when this exits (kling compose down removes them)")
	noLogs := fs.Bool("no-logs", false, "do not follow the services' output")
	stamps := fs.Bool("t", false, "show the time of each line of the services' output (RFC 3339, UTC)")
	fresh := fs.Bool("fresh", false, "rebuild the services' warm templates (after re-pulling an image tag, say)")
	_ = fs.Parse(reorderFor(fs, args))
	if fs.NArg() != 0 {
		return errors.New(composeUsage)
	}
	t, _, err := leerCompose(*file, *name)
	if err != nil {
		return err
	}
	printAvisos(t.Avisos)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c := api.NewClient(hostOf(*host))
	t0 := time.Now()

	// Un up de un proyecto que ya está: se sustituye (sus volúmenes siguen).
	_ = selfCmd(ctx, *host, "graph", "rm", "-f", t.Proyect).Run()
	quitarHuerfanas(ctx, c, t)
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
	if out, err := up.Output(); err != nil {
		return fmt.Errorf("graph up: %w", err)
	} else {
		_ = out
	}
	fmt.Fprintf(os.Stderr, "%s is up in %.1fs\n", t.Proyect, time.Since(t0).Seconds())

	var lns []net.Listener
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	for _, nodo := range clavesOrd(t.Publica) {
		for _, p := range t.Publica[nodo] {
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p[0])))
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: cannot publish 127.0.0.1:%d (%v)\n", nodo, p[0], err)
				continue
			}
			lns = append(lns, ln)
			fmt.Fprintf(os.Stderr, "  %-12s 127.0.0.1:%d -> %d\n", nodo, p[0], p[1])
			go publish(ctx, c, ln, t.Proyect+"-"+nodo, p[1])
		}
	}

	if !*noLogs {
		nodos := nodosDe(t)
		w := 0
		for _, n := range nodos {
			w = max(w, len(n))
		}
		var mu sync.Mutex
		for _, n := range nodos {
			go followServiceLogs(ctx, *host, t.Proyect+"-"+n, fmt.Sprintf("%-*s | ", w, n), *stamps, &mu)
		}
	}
	if *keep {
		fmt.Fprintln(os.Stderr, "Ctrl-C stops publishing and following; the machines stay (kling compose down removes them)")
	} else {
		fmt.Fprintln(os.Stderr, "Ctrl-C removes the machines (named volumes stay)")
	}
	<-ctx.Done()
	if *keep {
		return nil
	}
	fmt.Fprintf(os.Stderr, "\nremoving %s...\n", t.Proyect)
	return selfCmd(context.Background(), *host, "graph", "rm", "-f", t.Proyect).Run()
}

// quitarHuerfanas borra las máquinas <proyecto>-<nodo> de un grafo que ya
// no existe: un up que falló a medias puede dejar una fallida que el grafo no
// se llevó, y con ella el siguiente up falla por el nombre ("machine name is
// taken") hasta un kling rm a mano. Solo las que llevan la etiqueta de grafo:
// una máquina del usuario con ese nombre no se toca.
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
		if nombres[m.Name] && m.Labels[api.LabelGraph] != "" {
			fmt.Fprintf(os.Stderr, "removing %s, left %s by an earlier up\n", m.Name, m.State)
			_ = c.Remove(ctx, m.ID)
		}
	}
}

func composeDown(args []string) error {
	fs := flag.NewFlagSet("compose down", flag.ExitOnError)
	host := hostFlag(fs)
	file := fs.String("f", "", "compose file")
	name := fs.String("p", "", "project (graph) name")
	_ = fs.Parse(reorderFor(fs, args))
	proyecto := *name
	if proyecto == "" {
		t, _, err := leerCompose(*file, "")
		if err != nil {
			return err
		}
		proyecto = t.Proyect
	}
	cmd := selfCmd(context.Background(), *host, "graph", "rm", "-f", proyecto)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
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

// publish atiende 127.0.0.1:H y lleva cada conexión al puerto del invitado de
// la máquina, resolviendo su dirección en cada conexión.
func publish(ctx context.Context, c *api.Client, ln net.Listener, machine string, port int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			up, err := dialMachine(ctx, c, machine, port)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s:%d: %v\n", machine, port, err)
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			cp := func(dst, src net.Conn) {
				_, _ = io.Copy(dst, src)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				done <- struct{}{}
			}
			go cp(up, conn)
			go cp(conn, up)
			<-done
			<-done
		}()
	}
}

// dialMachine conecta con el puerto del invitado: despierta la máquina si
// está congelada o en pausa, y reintenta unos segundos si el servicio aún no
// escucha (acaba de arrancar).
func dialMachine(ctx context.Context, c *api.Client, machine string, port int) (net.Conn, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		m, err := c.Get(ctx, machine)
		if err != nil {
			return nil, err
		}
		if m.State == api.StateWarm || m.State == api.StatePaused {
			if m, err = c.Thaw(ctx, machine); err != nil {
				return nil, err
			}
		}
		conn, err := net.DialTimeout("tcp", m.Addr(port), 2*time.Second)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
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
