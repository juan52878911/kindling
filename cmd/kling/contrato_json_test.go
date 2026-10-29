package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/contrato"
	"github.com/juan52878911/kindling/pkg/api"
)

var actualizar = flag.Bool("update", false, "regenerate testdata/contrato/json.txt")

// comandosJSON son los comandos cuya salida -json es contrato
// (docs/compatibilidad.md): la forma (claves y tipos), no los valores.
var comandosJSON = [][]string{
	{"ps", "-a", "-json"},
	{"inspect", "m1"},
	{"image", "ls", "-json"},
	{"template", "ls", "-json"},
	{"volume", "ls", "-json"},
	{"graph", "ls", "-json"},
	{"sandbox", "ls", "-json"},
	{"status", "-json"},
	{"version", "-json"},
}

// La salida -json de los comandos principales, contra un daemon falso que
// contesta con cada campo de pkg/api relleno (rellenar), aplanada a
// "<comando>: <ruta JSON> <tipo>" y comparada con testdata/contrato/json.txt.
// Quitar o renombrar una clave es incompatible y exige subir api.APIVersion;
// una nueva pide -update:
//
//	go test ./cmd/kling -run TestContratoJSON -update
func TestContratoJSON(t *testing.T) {
	hermetic(t)
	sock := daemonRelleno(t)
	t.Setenv("KLING_HOST", "unix://"+sock)

	var lineas []string
	for _, argv := range comandosJSON {
		out, err := capturarStdout(t, func() error { return ejecutar(argv) })
		nombre := strings.Join(argv, " ")
		if err != nil {
			t.Fatalf("kling %s: %v", nombre, err)
		}
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatalf("kling %s no da JSON: %v\n%s", nombre, err, out)
		}
		for _, l := range aplanar("", v) {
			lineas = append(lineas, "kling "+nombre+":  "+l)
		}
	}
	cab := []string{
		"Forma de la salida -json del CLI: kling <comando>:  <ruta JSON> <tipo>.",
		"Lo genera TestContratoJSON (cmd/kling/contrato_json_test.go) contra un",
		"daemon falso con todos los campos rellenos; para regenerarlo:",
		"  go test ./cmd/kling -run TestContratoJSON -update",
		"Quitar o cambiar una línea exige subir api.APIVersion (internal/contrato).",
	}
	if err := contrato.Comparar("testdata/contrato/json.txt", api.APIVersion, cab, lineas, *actualizar); err != nil {
		t.Fatal(err)
	}
}

// ejecutar corre un comando del CLI como main, sin salir del proceso.
func ejecutar(argv []string) error {
	switch argv[0] {
	case "ps":
		return cmdPS(argv[1:])
	case "inspect":
		return cmdInspect(argv[1:])
	case "image":
		return cmdImages(argv[1:])
	case "template":
		return cmdTemplate(argv[1:])
	case "volume":
		return cmdVolume(argv[1:])
	case "graph":
		return cmdGraph(argv[1:])
	case "sandbox":
		return cmdSandbox(argv[1:])
	case "status":
		return cmdStatus(argv[1:])
	case "version":
		return cmdVersion(argv[1:])
	}
	return fmt.Errorf("comando %q sin enrutar en el test", argv[0])
}

// daemonRelleno sirve, en un socket Unix, las rutas que leen comandosJSON con
// valores de pkg/api que tienen todos sus campos rellenos.
func daemonRelleno(t *testing.T) string {
	t.Helper()
	m := http.NewServeMux()
	servir := func(patron string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", patron, err)
		}
		m.HandleFunc(patron, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		})
	}
	maquina := relleno[api.Machine]()
	maquina.ID, maquina.Name = "aaaaaaaaaaaaaaaa", "m1"
	servir("GET /info", relleno[api.Info]())
	servir("GET /machines", []*api.Machine{&maquina})
	servir("GET /machines/{ref}", &maquina)
	servir("GET /sandboxes", []*api.Machine{&maquina})
	servir("GET /images", []api.Image{relleno[api.Image]()})
	servir("GET /snapshots", []*api.Snapshot{punteroA(relleno[api.Snapshot]())})
	servir("GET /volumes", []*api.Volume{punteroA(relleno[api.Volume]())})
	servir("GET /graphs", []*api.Graph{punteroA(relleno[api.Graph]())})
	pub := relleno[api.Publication]()
	pub.Machine = maquina.ID // ps las casa por id
	servir("GET /publish", []api.Publication{pub})

	dir, err := os.MkdirTemp("/tmp", "kt")
	if err != nil {
		t.Skipf("no /tmp for a unix socket: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot listen on a unix socket: %v", err)
	}
	srv := &http.Server{Handler: m}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func punteroA[T any](v T) *T { return &v }

// relleno es un T con cada campo exportado distinto de su cero, para que
// ningún omitempty esconda una clave de la salida.
func relleno[T any]() T {
	var v T
	rellenar(reflect.ValueOf(&v).Elem(), 0)
	return v
}

var (
	tipoTiempo = reflect.TypeOf(time.Time{})
	tipoCrudo  = reflect.TypeOf(json.RawMessage(nil))
)

func rellenar(v reflect.Value, prof int) {
	if prof > 6 || !v.CanSet() {
		return
	}
	switch {
	case v.Type() == tipoTiempo:
		v.Set(reflect.ValueOf(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)))
		return
	case v.Type() == tipoCrudo:
		v.Set(reflect.ValueOf(json.RawMessage(`"x"`)))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		rellenar(p.Elem(), prof+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		rellenar(s.Index(0), prof+1)
		v.Set(s)
	case reflect.Map:
		mp := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		rellenar(k, prof+1)
		e := reflect.New(v.Type().Elem()).Elem()
		rellenar(e, prof+1)
		mp.SetMapIndex(k, e)
		v.Set(mp)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				rellenar(v.Field(i), prof+1)
			}
		}
	}
}

// aplanar describe un JSON como "<ruta> <tipo>" por hoja: "machines[].id
// string". Los arrays son [] (su primer elemento) y las claves de un mapa,
// las que dejó rellenar ("x").
func aplanar(ruta string, v any) []string {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			return []string{strings.TrimSpace(ruta + " {}")}
		}
		claves := make([]string, 0, len(x))
		for k := range x {
			claves = append(claves, k)
		}
		sort.Strings(claves)
		var out []string
		for _, k := range claves {
			sub := k
			if ruta != "" {
				sub = ruta + "." + k
			}
			out = append(out, aplanar(sub, x[k])...)
		}
		return out
	case []any:
		if len(x) == 0 {
			return []string{ruta + "[] (empty)"}
		}
		return aplanar(ruta+"[]", x[0])
	case string:
		return []string{ruta + " string"}
	case float64:
		return []string{ruta + " number"}
	case bool:
		return []string{ruta + " bool"}
	case nil:
		return []string{ruta + " null"}
	}
	return []string{fmt.Sprintf("%s %T", ruta, v)}
}

// capturarStdout corre f con os.Stdout apuntando a una tubería y devuelve lo
// que escribió.
func capturarStdout(t *testing.T, f func() error) ([]byte, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	viejo := os.Stdout
	os.Stdout = w
	leido := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		leido <- b
	}()
	ferr := f()
	os.Stdout = viejo
	w.Close()
	return <-leido, ferr
}
