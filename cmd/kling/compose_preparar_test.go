package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// F4: compose up importa lo que no reemplaza nada ANTES de quitar el
// proyecto levantado. Un pull que falla lo deja como estaba; un import que
// necesita -replace va después de quitarlo.
func TestUpNoDerribaSiFallaElImport(t *testing.T) {
	var mu sync.Mutex
	var pasos []string
	apunta := func(s string) {
		mu.Lock()
		pasos = append(pasos, s)
		mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /graphs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		apunta("graph rm " + r.PathValue("ref"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*api.Machine{})
	})
	c := fakeDaemon(t, mux)
	tr := traducirParaPlantillas(t, "services:\n  db:\n    image: postgres:17\n  app:\n    image: nginx\n")
	vivo := &vistaProyecto{g: &api.Graph{ID: "g1", Name: tr.Proyect}, ms: []*api.Machine{}}

	plan := func(run func(context.Context, string) (string, error)) *preparacionCompose {
		p := &preparacionCompose{t: tr, c: c, run: run}
		for _, s := range tr.vivos {
			p.ps = append(p.ps, &plantillaCompose{s: s})
			p.imports = append(p.imports, importCompose{linea: ordenImportCon(s, tr.Proyect, true, true, false),
				conReplace: ordenImportCon(s, tr.Proyect, true, true, true)})
		}
		return p
	}

	// Un pull que falla: ni se quita el grafo ni se corre nada con -replace.
	pasos = nil
	p := plan(func(_ context.Context, l string) (string, error) {
		apunta(l)
		if strings.Contains(l, "nginx") {
			return "pull nginx: manifest unknown", errors.New("exit status 1")
		}
		return "", nil
	})
	err := sustituirProyecto(context.Background(), c, tr, vivo, p, false)
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("err = %v", err)
	}
	for _, s := range pasos {
		if strings.HasPrefix(s, "graph rm") || strings.Contains(s, "-replace") {
			t.Fatalf("con el import fallido hizo %q (pasos %q)", s, pasos)
		}
	}

	// La receta de db cambió: su -replace va después de quitar el grafo.
	pasos = nil
	p = plan(func(_ context.Context, l string) (string, error) {
		apunta(l)
		if strings.Contains(l, "postgres") && !strings.Contains(l, "-replace") {
			return "image dev-db was imported with other options; use -replace", errors.New("exit status 1")
		}
		return "", nil
	})
	if err := sustituirProyecto(context.Background(), c, tr, vivo, p, false); err != nil {
		t.Fatal(err)
	}
	rm := slices.Index(pasos, "graph rm "+tr.Proyect)
	rep := slices.IndexFunc(pasos, func(s string) bool { return strings.Contains(s, "-replace") })
	if rm < 0 || rep < rm {
		t.Fatalf("orden: %q", pasos)
	}
}

// R11: los imports van como mucho paraleloCompose a la vez, y el primer
// fallo corta los demás: los que faltan no empiezan.
func TestImportsAcotadosYCortan(t *testing.T) {
	var imports []importCompose
	for i := range 30 {
		imports = append(imports, importCompose{linea: "kling image import x -name n" + string(rune('a'+i))})
	}
	var dentro, maxDentro, llamadas atomic.Int32
	run := func(ctx context.Context, l string) (string, error) {
		llamadas.Add(1)
		n := dentro.Add(1)
		defer dentro.Add(-1)
		for {
			m := maxDentro.Load()
			if n <= m || maxDentro.CompareAndSwap(m, n) {
				break
			}
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if strings.HasSuffix(l, "nc") {
			return "boom", errors.New("exit status 1")
		}
		return "", nil
	}
	_, err := importarSinReemplazar(context.Background(), run, imports)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if maxDentro.Load() > paraleloCompose {
		t.Errorf("%d a la vez (tope %d)", maxDentro.Load(), paraleloCompose)
	}
	if llamadas.Load() >= 30 {
		t.Errorf("tras el fallo siguieron todos: %d llamadas", llamadas.Load())
	}
}

// R11: como mucho paraleloPlantillas máquinas de preparación a la vez, y un
// fallo no deja arrancar las que faltan.
func TestPlantillasAcotadas(t *testing.T) {
	ops := &opsContadas{falla: "s3"}
	var ps []*plantillaCompose
	for i := range 12 {
		n := "s" + string(rune('0'+i%10)) + string(rune('a'+i))
		ps = append(ps, &plantillaCompose{s: &servicioCompose{nombre: n, nodo: n}, nombre: "tpl-" + n})
	}
	err := prepararPlantillas(context.Background(), ops, "dev", ps, &strings.Builder{})
	if err == nil {
		t.Fatal("sin error")
	}
	if ops.max.Load() > paraleloPlantillas {
		t.Errorf("%d a la vez (tope %d)", ops.max.Load(), paraleloPlantillas)
	}
	if ops.runs.Load() >= 12 {
		t.Errorf("tras el fallo arrancaron todas: %d", ops.runs.Load())
	}
}

type opsContadas struct {
	falla       string
	dentro, max atomic.Int32
	runs        atomic.Int32
}

func (o *opsContadas) Run(ctx context.Context, r api.RunRequest) (*api.Machine, error) {
	o.runs.Add(1)
	n := o.dentro.Add(1)
	defer o.dentro.Add(-1)
	for {
		m := o.max.Load()
		if n <= m || o.max.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if strings.HasPrefix(r.Name, "tpl-"+o.falla) {
		return nil, errors.New("no memory")
	}
	return &api.Machine{Name: r.Name, Ready: api.ReadyYes}, nil
}

func (o *opsContadas) Exec(context.Context, string, api.ExecRequest, func(string, []byte) error) (*api.ExecResult, error) {
	return &api.ExecResult{}, nil
}

func (o *opsContadas) CommitWith(context.Context, string, api.CommitRequest) (*api.Snapshot, error) {
	return &api.Snapshot{}, nil
}

func (o *opsContadas) Remove(context.Context, string) error { return nil }

// S21b: el entorno de un servicio va a kling image import en un -env-file y
// no en argv (lo ve cualquiera con ps), y llega igual. S14: el fichero no
// toca el disco (llega por un pipe), y un valor que no cabe en él va por el
// entorno del hijo sin arrastrar a los demás de vuelta a argv.
func TestEntornoAFichero(t *testing.T) {
	s := &servicioCompose{nodo: "db", imagen: "postgres:17", user: "-e", entry: []string{"-e"},
		env: [][2]string{{"DB_PASS", "s3cr #et"}, {"DSN", "a=b: c"}, {"VACIA", ""}, {"CERT", "-----BEGIN\nfin "}},
		cmd: []string{"-e", "X=1"}}
	o, err := argvKling(ordenImport(s, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range o.argv {
		if strings.Contains(a, "s3cr") || strings.Contains(a, "a=b") || strings.Contains(a, "BEGIN") {
			t.Fatalf("un secreto sigue en argv: %q", o.argv)
		}
	}
	// -user y -entrypoint conservan su valor ("-e"), y lo de detrás de --,
	// también; el -env-file va antes del --.
	k := slices.Index(o.argv, "-env-file")
	if k < 0 || o.argv[k+1] != fdEntorno || !slices.Equal(o.argv[k+2:], []string{"--", "-e", "X=1"}) || o.argv[slices.Index(o.argv, "-user")+1] != "-e" {
		t.Fatalf("argv = %q", o.argv)
	}
	if !slices.Contains(o.argv, "CERT") || !slices.Equal(o.env, []string{"CERT=-----BEGIN\nfin "}) {
		t.Fatalf("CERT: argv %q env %q", o.argv, o.env)
	}
	got, err := resolveEnv(nil, []string{"f"}, nil, func(string) ([]byte, error) { return o.fichero, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"DB_PASS=s3cr #et", "DSN=a=b: c", "VACIA="}) {
		t.Fatalf("entorno = %q", got)
	}
	// Uno que no puede ir por el entorno del hijo (cambiaría a qué daemon
	// habla) se queda en argv, solo él.
	sin, lineas, env, ok := entornoAFichero([]string{"image", "import", "x", "-e", "KLING_HOST=a\nb", "-e", "P=s"})
	if !ok || !slices.Equal(sin, []string{"image", "import", "x", "-e", "KLING_HOST=a\nb"}) || !slices.Equal(lineas, []string{"P=s"}) || env != nil {
		t.Fatalf("sin %q lineas %q env %q", sin, lineas, env)
	}
}

// S14: el hijo lee el -env-file de /dev/fd/3 y su entorno, y no queda
// ningún fichero en el directorio temporal mientras corre.
func TestCorrerLineaSinFichero(t *testing.T) {
	if _, err := os.Stat(fdEntorno); err != nil {
		t.Skipf("sin /dev/fd: %v", err)
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	o := &ordenKling{fichero: []byte("A=1\nB=dos\n"), env: []string{"CERT=x\ny"}}
	cmd := exec.Command("sh", "-c", `ls -A "$TMPDIR"; cat `+fdEntorno+`; printf '%s' "$CERT"`)
	out, err := correrLinea(cmd, o)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out != "A=1\nB=dos\nx\ny" {
		t.Fatalf("salida = %q", out)
	}
}
