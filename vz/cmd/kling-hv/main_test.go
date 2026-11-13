//go:build darwin && arm64

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/hvvm"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// Estos tests no crean ninguna VM: miran lo que kling-hv hace antes de tocar
// Hypervisor.framework (argumentos, entorno, rutas) y su salida.

// capturar cambia os.Stdout y os.Stderr por tuberías mientras corre f.
func capturar(t *testing.T, f func()) (salida, errores string) {
	t.Helper()
	leer := func(dst *bytes.Buffer, r *os.File, wg *sync.WaitGroup) {
		defer wg.Done()
		_, _ = io.Copy(dst, r)
	}
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var bo, be bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go leer(&bo, ro, &wg)
	go leer(&be, re, &wg)
	viejoOut, viejoErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wo, we
	defer func() {
		os.Stdout, os.Stderr = viejoOut, viejoErr
		wo.Close()
		we.Close()
		wg.Wait()
		ro.Close()
		re.Close()
		salida, errores = bo.String(), be.String()
	}()
	f()
	return
}

// correr ejecuta run() con esos argumentos.
func correr(t *testing.T, args ...string) (codigo int, salida, errores string) {
	t.Helper()
	viejos := os.Args
	os.Args = append([]string{"kling-hv"}, args...)
	defer func() { os.Args = viejos }()
	salida, errores = capturar(t, func() { codigo = run() })
	return
}

func TestArgumentosQueSalenSinVM(t *testing.T) {
	min, max := spec.Formatos()
	casos := []struct {
		nombre  string
		args    []string
		codigo  int
		salida  string // la salida entera, si no es ""
		errores string // un fragmento de stderr, si no es ""
	}{
		{"versión", []string{"--version"}, 0, "kling-hv " + version + "\n", ""},
		{"versión con un guion", []string{"-version"}, 0, "kling-hv " + version + "\n", ""},
		{"versión aunque venga --id", []string{"--id", "abc", "--version"}, 0, "kling-hv " + version + "\n", ""},
		// Tercer número: la versión del pie de mem.file, solo de kling-hv.
		{"formatos", []string{"--snapshot-formats"}, 0, fmt.Sprintln(min, max, hvvm.VersionVolcado), ""},
		{"sin socket", nil, 2, "", "--api-sock is required"},
		{"solo --id", []string{"--id", "abc"}, 2, "", "--api-sock is required"},
		{"socket vacío", []string{"--api-sock", ""}, 2, "", "--api-sock is required"},
		{"bandera desconocida", []string{"--no-existe"}, 2, "", "flag provided but not defined"},
		{"argumento sin valor", []string{"--api-sock"}, 2, "", "flag needs an argument"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			codigo, salida, errores := correr(t, c.args...)
			if codigo != c.codigo {
				t.Fatalf("código %d, quiero %d (stdout %q, stderr %q)", codigo, c.codigo, salida, errores)
			}
			if c.salida != "" && salida != c.salida {
				t.Errorf("stdout = %q, quiero %q", salida, c.salida)
			}
			if c.errores != "" && !strings.Contains(errores, c.errores) {
				t.Errorf("stderr = %q, sin %q", errores, c.errores)
			}
		})
	}
}

// Un entorno que no se entiende para el arranque antes de abrir el socket: código 2 y nada creado junto al socket.
func TestEntornoInvalidoNoArranca(t *testing.T) {
	for _, c := range []struct{ variable, valor string }{
		{"KLING_VZ_VSOCK", "si"},
		{"KLING_VZ_VSOCK", "2"},
		{"KLING_VZ_MTU", "abc"},
		{"KLING_VZ_MTU", "100"},
		{"KLING_VZ_MTU", "999999"},
		{"KLING_VZ_MTU", "-1"},
	} {
		t.Run(c.variable+"="+c.valor, func(t *testing.T) {
			for _, v := range []string{"KLING_VZ_VSOCK", "KLING_VZ_MTU"} {
				t.Setenv(v, "")
			}
			t.Setenv(c.variable, c.valor)
			dir := t.TempDir()
			sock := filepath.Join(dir, "api.sock")
			if codigo, _, _ := correr(t, "--api-sock", sock); codigo != 2 {
				t.Fatalf("código %d, quiero 2", codigo)
			}
			if es, _ := os.ReadDir(dir); len(es) != 0 {
				t.Errorf("quedó algo junto al socket: %v", es)
			}
		})
	}
}

// line empieza siempre en columna 0, aunque la consola del invitado dejara
// la línea a medias (un prompt sin salto de línea).
func TestSyncWriterLineaEnColumnaCero(t *testing.T) {
	var b bytes.Buffer
	w := &syncWriter{w: &b}
	w.line("kling-hv: primera")
	w.Write([]byte("login: "))
	w.line("kling-hv: tras el prompt")
	w.Write([]byte("una línea entera\n"))
	w.line("kling-hv: tras una línea")
	w.Write(nil) // vacío: no cambia lo último escrito
	w.line("kling-hv: tras nada")
	quiero := "kling-hv: primera\nlogin: \nkling-hv: tras el prompt\nuna línea entera\nkling-hv: tras una línea\nkling-hv: tras nada\n"
	if b.String() != quiero {
		t.Fatalf("salida:\n%q\nquiero:\n%q", b.String(), quiero)
	}
}

// Escrituras de la consola y diagnósticos a la vez no se mezclan a mitad de
// línea (con -race, además, no hay carrera).
func TestSyncWriterConcurrente(t *testing.T) {
	var b bytes.Buffer
	w := &syncWriter{w: &b}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.line("kling-hv: diagnóstico")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Write([]byte("consola\n"))
			}
		}()
	}
	wg.Wait()
	for _, l := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
		if l != "kling-hv: diagnóstico" && l != "consola" {
			t.Fatalf("línea mezclada: %q", l)
		}
	}
}

func TestRutaAuditoria(t *testing.T) {
	real := t.TempDir()
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r // en macOS el TempDir cuelga de /var, que es /private/var
	}
	enlace := filepath.Join(t.TempDir(), "enlace")
	if err := os.Symlink(real, enlace); err != nil {
		t.Fatal(err)
	}
	quiero := filepath.Join(real, credproxy.AuditFileVMM)
	if got := rutaAuditoria(filepath.Join(enlace, "api.sock")); got != quiero {
		t.Errorf("por un enlace: %q, quiero %q", got, quiero)
	}
	if got := rutaAuditoria(filepath.Join(real, "api.sock")); got != quiero {
		t.Errorf("directa: %q, quiero %q", got, quiero)
	}
	// Relativa: absoluta respecto al directorio de trabajo, que el proceso
	// cambia después con una ruta de socket larga.
	t.Chdir(real)
	if got := rutaAuditoria("api.sock"); got != quiero {
		t.Errorf("relativa: %q, quiero %q", got, quiero)
	}
	// Un directorio que aún no existe se queda como está, pero absoluto.
	if got := rutaAuditoria(filepath.Join(real, "no", "api.sock")); got != filepath.Join(real, "no", credproxy.AuditFileVMM) {
		t.Errorf("sin directorio: %q", got)
	}
}

func TestRutaBroker(t *testing.T) {
	real := t.TempDir()
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r
	}
	enlace := filepath.Join(t.TempDir(), "enlace")
	if err := os.Symlink(real, enlace); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ valor, quiero string }{
		{"", ""},
		{"broker.sock", ""},     // relativa: no
		{"./x/broker.sock", ""}, // relativa: no
		{"../broker.sock", ""},  // relativa: no
		{filepath.Join(real, "broker.sock"), filepath.Join(real, "broker.sock")},
		// El directorio se resuelve (el sandbox compara rutas reales); el
		// nombre del socket no, que puede no existir aún.
		{filepath.Join(enlace, "broker.sock"), filepath.Join(real, "broker.sock")},
		{filepath.Join(real, "no-existe", "broker.sock"), filepath.Join(real, "no-existe", "broker.sock")},
	} {
		t.Setenv("KLING_VZ_BROKER", c.valor)
		if got := rutaBroker(); got != c.quiero {
			t.Errorf("KLING_VZ_BROKER=%q: %q, quiero %q", c.valor, got, c.quiero)
		}
	}
}

// Un *grafo.Grafo nil dentro de la interfaz no sería nil para vnet.
func TestGrafoDeRedNil(t *testing.T) {
	if g := grafoDeRed(nil); g != nil {
		t.Fatalf("grafoDeRed(nil) = %#v, quiero una interfaz nil", g)
	}
}
