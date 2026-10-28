//go:build darwin

package sandbox

// Los perfiles de kling-vz y kling-hv, aplicados de verdad: sandbox_init no
// tiene vuelta atrás, así que cada prueba se encierra en un proceso hijo (este
// mismo binario de test, ver TestMain) que dice cuánto tardó sandbox_init y
// qué le dejó hacer la red.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/server"
)

const envHijo = "KLING_SANDBOX_TEST_HIJO"

// orden es lo que el hijo recibe: el perfil, sus parámetros y qué probar.
type orden struct {
	Perfil  string
	Pares   []string
	Pruebas []prueba
}

// prueba: Op "bind" (escuchar en Addr), "connect" (conectar a Addr por TCP),
// "unix" (conectar al socket Unix Addr), "read" o "write" (el fichero Addr).
type prueba struct{ Op, Addr string }

// resultado: cuánto tardó sandbox_init y, por prueba, "ok", "denied" (EPERM:
// el sandbox) u otro error.
type resultado struct {
	InitNs int64
	Res    []string
}

func TestMain(m *testing.M) {
	if o := os.Getenv(envHijo); o != "" {
		os.Exit(hijo(o))
	}
	os.Exit(m.Run())
}

func hijo(s string) int {
	var o orden
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	t0 := time.Now()
	if err := Aplicar(o.Perfil, o.Pares); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox_init:", err)
		return 3
	}
	r := resultado{InitNs: time.Since(t0).Nanoseconds()}
	for _, p := range o.Pruebas {
		var err error
		switch p.Op {
		case "bind":
			var l net.Listener
			if l, err = net.Listen("tcp", p.Addr); err == nil {
				l.Close()
			}
		case "read":
			var b []byte
			b, err = os.ReadFile(p.Addr)
			_ = b
		case "write":
			var f *os.File
			if f, err = os.OpenFile(p.Addr, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
				f.Close()
			}
		case "connect", "unix":
			red := "tcp"
			if p.Op == "unix" {
				red = "unix"
			}
			var c net.Conn
			if c, err = net.DialTimeout(red, p.Addr, 300*time.Millisecond); err == nil {
				c.Close()
			}
		}
		switch {
		case err == nil:
			r.Res = append(r.Res, "ok")
		case errors.Is(err, syscall.EPERM):
			r.Res = append(r.Res, "denied")
		default:
			r.Res = append(r.Res, err.Error())
		}
	}
	b, _ := json.Marshal(r)
	os.Stdout.Write(b)
	return 0
}

// encerrado corre o en un hijo y devuelve lo que contó.
func encerrado(t testing.TB, o orden) resultado {
	t.Helper()
	b, _ := json.Marshal(o)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), envHijo+"="+string(b))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hijo: %v", err)
	}
	var r resultado
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("hijo: %v: %q", err, out)
	}
	return r
}

// perfiles son los dos perfiles de verdad, leídos de sus paquetes.
func perfiles(t testing.TB) map[string]string {
	t.Helper()
	p := map[string]string{}
	for _, n := range []string{"kling-vz", "kling-hv"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "cmd", n, n+".sb"))
		if err != nil {
			t.Fatal(err)
		}
		p[n] = string(b)
	}
	return p
}

// pares son los parámetros de una VM con esa red, en directorios de prueba.
func pares(t testing.TB, conRed bool, broker string) []string {
	t.Helper()
	root := Real(t.TempDir())
	mdir := filepath.Join(root, "machines", "m")
	if err := os.MkdirAll(mdir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := Parametros(root, mdir, broker, server.Confinamiento{ConRed: conRed, Loopback: conRed})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// puertoLibre busca un puerto de 127.0.0.1 libre en [desde, hasta].
func puertoLibre(t testing.TB, desde, hasta int) int {
	t.Helper()
	for p := desde; p <= hasta; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			l.Close()
			return p
		}
	}
	t.Skipf("no free port in %d-%d", desde, hasta)
	return 0
}

// escucha abre un puerto de 127.0.0.1 en [desde, hasta] que acepta y cierra.
func escucha(t testing.TB, desde, hasta int) string {
	t.Helper()
	for p := desde; p <= hasta; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		return l.Addr().String()
	}
	t.Skipf("no free port in %d-%d", desde, hasta)
	return ""
}

// La política de red de los dos perfiles: los reenvíos se abren en 127.0.0.1
// (cualquier puerto); al loopback del Mac no se conecta nunca, sea cual sea la
// red (el rango de los reenvíos de las demás máquinas incluido: el upstream
// del loopback de una credencial lo marca el daemon); al exterior solo con
// red; y de los sockets Unix de fuera, solo el del broker.
func TestPerfilRed(t *testing.T) {
	fmin, fmax := credproxy.ForwardPortMin, credproxy.ForwardPortMax
	// Conectar a un puerto que el sandbox niega da EPERM antes de llegar a la
	// red, haya quien escuche o no: el rango no necesita escuchas.
	rango := func(p int) string { return "127.0.0.1:" + strconv.Itoa(p) }
	fuera := escucha(t, 40000, 40999) // un servicio del Mac (127.0.0.1:5432...)
	bindRango := rango(puertoLibre(t, fmin+100, fmax))
	bindFuera := rango(puertoLibre(t, 41000, 41999))
	// Un socket Unix fuera del directorio de la máquina y el del broker.
	dir, err := os.MkdirTemp("/tmp", "sbt") // /tmp es /private/tmp: se resuelve, como hace el VMM
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir = Real(dir)
	otro, broker := filepath.Join(dir, "otro.sock"), filepath.Join(dir, "broker.sock")
	for _, p := range []string{otro, broker} {
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
	}

	pruebas := []prueba{
		{"connect", rango(fmin + 500)}, {"connect", rango(fmin)}, {"connect", rango(fmax)},
		{"connect", fuera}, {"connect", rango(fmax + 1)}, {"connect", rango(fmin - 1)},
		{"connect", "0.0.0.0:" + strconv.Itoa(fmin)}, {"connect", "[::1]:" + strconv.Itoa(fmin)},
		{"bind", bindRango}, {"bind", bindFuera},
		{"unix", otro}, {"unix", broker},
		{"connect", "192.0.2.1:9"}, // TEST-NET-1: nadie contesta, pero es el exterior
	}
	for nombre, perfil := range perfiles(t) {
		for _, red := range []struct {
			nombre string
			conRed bool
		}{{"none", false}, {"internet", true}, {"allowlist", true}} {
			t.Run(nombre+"/"+red.nombre, func(t *testing.T) {
				r := encerrado(t, orden{Perfil: perfil, Pares: pares(t, red.conRed, broker), Pruebas: pruebas})
				exterior := "denied"
				if red.conRed {
					exterior = "allowed"
				}
				quiere := []string{"denied", "denied", "denied", "denied", "denied", "denied", "denied", "denied",
					"allowed", "allowed", "denied", "allowed", exterior}
				// Lo que no es EPERM lo dejó pasar el sandbox (un connection
				// refused de un puerto sin escucha, o un plazo, también).
				for i, x := range r.Res {
					if x != "denied" {
						r.Res[i] = "allowed"
					}
				}
				if !slices.Equal(r.Res, quiere) {
					t.Fatalf("pruebas %v\n got  %v\n want %v", pruebas, r.Res, quiere)
				}
			})
		}
	}
}

// Los ficheros: su directorio, los L para leer y los E para leer y escribir;
// nada más de la raíz, ni secrets/, ni escribir en snapshots/.
func TestPerfilFicheros(t *testing.T) {
	root := Real(t.TempDir())
	mdir := filepath.Join(root, "machines", "m")
	otra := filepath.Join(root, "machines", "otra")
	for _, d := range []string{mdir, otra, filepath.Join(root, "secrets"), filepath.Join(root, "snapshots", "g"), filepath.Join(root, "images")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := func(p string) string {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kernel, disco := f(filepath.Join(root, "images", "vmlinux")), f(filepath.Join(root, "images", "base.ext4"))
	mem, vol := f(filepath.Join(root, "snapshots", "g", "mem.file")), f(filepath.Join(root, "volumes-v.ext4"))
	otroDisco := f(filepath.Join(root, "images", "otra.ext4"))
	clave, ajeno := f(filepath.Join(root, "secrets", "snapshot.key")), f(filepath.Join(otra, "overlay.ext4"))
	c := server.Confinamiento{Lectura: []string{kernel, disco, mem}, Escritura: []string{vol}}
	pares, err := Parametros(root, mdir, "", c)
	if err != nil {
		t.Fatal(err)
	}
	pruebas := []prueba{
		{"read", kernel}, {"read", disco}, {"read", mem}, {"read", vol}, {"write", vol},
		{"write", filepath.Join(mdir, "fc.sock.tmp")}, {"read", filepath.Join(mdir, "fc.sock.tmp")},
		{"write", disco}, {"write", mem}, {"read", otroDisco}, {"read", clave}, {"read", ajeno},
		{"write", filepath.Join(root, "snapshots", "g", "snap.file")}, {"write", filepath.Join(root, "x")},
	}
	quiere := []string{"ok", "ok", "ok", "ok", "ok", "ok", "ok",
		"denied", "denied", "denied", "denied", "denied", "denied", "denied"}
	for nombre, perfil := range perfiles(t) {
		t.Run(nombre, func(t *testing.T) {
			r := encerrado(t, orden{Perfil: perfil, Pares: pares, Pruebas: pruebas})
			if !slices.Equal(r.Res, quiere) {
				t.Fatalf("pruebas %v\n got  %v\n want %v", pruebas, r.Res, quiere)
			}
		})
	}
}

// BenchmarkSandboxInit mide sandbox_init_with_parameters de cada perfil y
// red, en un proceso nuevo cada vez (como un VMM). ms/init es la mediana.
func BenchmarkSandboxInit(b *testing.B) {
	for nombre, perfil := range perfiles(b) {
		for _, red := range []struct {
			nombre string
			conRed bool
		}{{"none", false}, {"allowlist", true}} {
			b.Run(nombre+"/"+red.nombre, func(b *testing.B) {
				p := pares(b, red.conRed, "")
				var v []float64
				for b.Loop() {
					r := encerrado(b, orden{Perfil: perfil, Pares: p})
					v = append(v, float64(r.InitNs)/1e6)
				}
				slices.Sort(v)
				b.ReportMetric(v[(len(v)-1)/2], "ms/init")
			})
		}
	}
}
