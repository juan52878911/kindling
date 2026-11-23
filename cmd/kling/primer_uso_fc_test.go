package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// tgzFC arma un tgz con la forma del de Firecracker: release-<v>-<arch>/ con
// firecracker-<v>-<arch> y jailer-<v>-<arch>. extra son entradas más (ruta → contenido).
func tgzFC(t *testing.T, ver, arch string, extra map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	dir := "release-" + ver + "-" + arch + "/"
	if extra == nil {
		add(dir+"firecracker-"+ver+"-"+arch, "#!/bin/sh\necho fc\n")
		add(dir+"jailer-"+ver+"-"+arch, "#!/bin/sh\necho jailer\n")
	}
	for n, b := range extra {
		add(n, b)
	}
	tw.Close()
	gz.Close()
	s := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(s[:])
}

func servirTgz(t *testing.T, b []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func descargaDe(srv *httptest.Server, dir, sha string) fcDescarga {
	return fcDescarga{URL: srv.URL + "/fc.tgz", SHA256: sha, Version: "v9.9.9", Arch: "x86_64",
		Dir: dir, Cliente: srv.Client(), MaxBytes: 1 << 20, Plazo: 5 * time.Second}
}

func TestFCDescargaBuenaInstalaLosDos(t *testing.T) {
	b, sha := tgzFC(t, "v9.9.9", "x86_64", nil)
	dir := t.TempDir()
	if err := descargarFirecracker(context.Background(), descargaDe(servirTgz(t, b), dir, sha)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"firecracker", "jailer"} {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", n, fi, err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("temporales sin limpiar: %v", ents)
	}
}

func TestFCShaCambiadoNoInstalaNada(t *testing.T) {
	b, sha := tgzFC(t, "v9.9.9", "x86_64", nil)
	dir := t.TempDir()
	mal := "0" + sha[1:]
	err := descargarFirecracker(context.Background(), descargaDe(servirTgz(t, b), dir, mal))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("dejó %v", ents)
	}
}

func TestFCRutasConPuntosPuntoNoEscapan(t *testing.T) {
	raiz := t.TempDir()
	dir := filepath.Join(raiz, "destino")
	os.Mkdir(dir, 0o755)
	b, sha := tgzFC(t, "v9.9.9", "x86_64", map[string]string{
		"../firecracker":                                     "malo",
		"release-v9.9.9-x86_64/../../jailer":                 "malo",
		"release-v9.9.9-x86_64/../firecracker-v9.9.9-x86_64": "malo",
	})
	err := descargarFirecracker(context.Background(), descargaDe(servirTgz(t, b), dir, sha))
	if err == nil {
		t.Fatal("un tgz sin los binarios en su sitio no puede instalarse")
	}
	for _, p := range []string{filepath.Join(raiz, "firecracker"), filepath.Join(raiz, "jailer"), filepath.Join(dir, "firecracker"), filepath.Join(dir, "jailer")} {
		if _, e := os.Stat(p); e == nil {
			t.Fatalf("escribió %s", p)
		}
	}
}

func TestFCTopeDeTamano(t *testing.T) {
	b, sha := tgzFC(t, "v9.9.9", "x86_64", nil)
	d := descargaDe(servirTgz(t, b), t.TempDir(), sha)
	d.MaxBytes = int64(len(b)) - 1
	if err := descargarFirecracker(context.Background(), d); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
}

// La tabla de aquí y la de scripts/20-install-firecracker.sh son la misma.
func TestFCTablaIgualQueElScript(t *testing.T) {
	b, err := os.ReadFile("../../scripts/20-install-firecracker.sh")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(v[0-9.]+)-(x86_64|aarch64)\)\s+FC_SHA256=([0-9a-f]{64})`)
	ms := re.FindAllStringSubmatch(string(b), -1)
	if len(ms) == 0 || len(ms) != len(fcPins) {
		t.Fatalf("el script tiene %d pares y la tabla %d", len(ms), len(fcPins))
	}
	for _, m := range ms {
		var ok bool
		for _, p := range fcPins {
			if p.Version == m[1] && p.Arch == m[2] && p.SHA256 == m[3] {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s %s %s no está en fcPins", m[1], m[2], m[3])
		}
	}
	if !strings.Contains(string(b), `FC_VERSION="${FC_VERSION:-`+fcVersion+`}"`) {
		t.Errorf("fcVersion %s no es la versión por defecto del script", fcVersion)
	}
}

type anotaciones struct{ cmds []string }

func (a *anotaciones) run(name string, args ...string) error {
	a.cmds = append(a.cmds, name+" "+strings.Join(args, " "))
	return nil
}

func TestHostLinuxInstallaLoQueFalta(t *testing.T) {
	var a anotaciones
	var out bytes.Buffer
	bajado := false
	o := opcionesHostLinux{out: &out, run: a.run,
		lookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		userExists: func(string) bool { return false },
		bajar:      func(context.Context) error { bajado = true; return nil },
		runAs:      "kindling", buildAs: "kindling-build"}
	if err := prepararHostLinux(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !bajado {
		t.Error("no bajó firecracker")
	}
	got := strings.Join(a.cmds, "\n")
	for _, w := range []string{"useradd --system --no-create-home --shell /usr/sbin/nologin kindling\n", "useradd --system --no-create-home --shell /usr/sbin/nologin kindling-build", "usermod -aG kvm kindling"} {
		if !strings.Contains(got+"\n", w) {
			t.Errorf("falta %q en\n%s", w, got)
		}
	}
	if !strings.Contains(out.String(), "firecracker") || !strings.Contains(out.String(), "useradd") {
		t.Errorf("cada paso se imprime:\n%s", out.String())
	}
}

func TestHostLinuxConTodoNoHaceNada(t *testing.T) {
	var a anotaciones
	o := opcionesHostLinux{out: &bytes.Buffer{}, run: a.run,
		lookPath:   func(n string) (string, error) { return "/usr/local/bin/" + n, nil },
		userExists: func(string) bool { return true },
		bajar:      func(context.Context) error { t.Error("bajó teniéndolos"); return nil },
		runAs:      "kindling", buildAs: "kindling-build"}
	if err := prepararHostLinux(context.Background(), o); err != nil || len(a.cmds) != 0 {
		t.Fatalf("err=%v cmds=%v", err, a.cmds)
	}
}

func TestHostLinuxDryRunNoHaceNada(t *testing.T) {
	var a anotaciones
	var out bytes.Buffer
	o := opcionesHostLinux{dryRun: true, out: &out, run: a.run,
		lookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		userExists: func(string) bool { return false },
		bajar:      func(context.Context) error { t.Error("bajó en dry-run"); return nil },
		runAs:      "kindling", buildAs: "kindling-build"}
	if err := prepararHostLinux(context.Background(), o); err != nil || len(a.cmds) != 0 {
		t.Fatalf("err=%v cmds=%v", err, a.cmds)
	}
	if !strings.Contains(out.String(), "useradd") || !strings.Contains(out.String(), "firecracker") {
		t.Errorf("el dry-run dice lo que haría:\n%s", out.String())
	}
}
