package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/dorado"
	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
)

const script81 = "../../scripts/81-base-image.sh"

// baseDePrueba escribe una base ext4 pequeña con lo que mira el copy-up: un
// /usr/local con permisos y grupo raros (para ver que suben tal cual), un
// /etc/resolv.conf del host y un /opt.
func baseDePrueba(t *testing.T, dir string) string {
	t.Helper()
	tt := time.Unix(1_700_000_000, 0)
	root := ext4.NewDir(0o755, 0, 0, tt)
	for _, d := range []struct {
		p        string
		perm     uint32
		uid, gid uint32
	}{{"/usr", 0o755, 0, 0}, {"/usr/local", 0o2775, 0, 50}, {"/usr/local/bin", 0o755, 0, 50},
		{"/etc", 0o755, 0, 0}, {"/opt", 0o755, 0, 0}, {"/var/log", 0o755, 0, 0}} {
		n, err := root.MkdirAll(d.p, 0o755, 0, 0, tt)
		if err != nil {
			t.Fatal(err)
		}
		n.Mode, n.UID, n.GID = ext4.ModeDir|d.perm, d.uid, d.gid
	}
	rc := "nameserver 127.0.0.53\n"
	root.Lookup("/etc").SetChild("resolv.conf", &ext4.Node{Mode: ext4.ModeReg | 0o640, GID: 7, Mtime: tt, Size: int64(len(rc)), Data: ext4.Bytes(rc)})
	p := filepath.Join(dir, "glibc.ext4")
	if _, err := writeExt4(p, root, nil, ext4.Options{Time: tt, LostFound: true}); err != nil {
		t.Fatal(err)
	}
	return p
}

// rootfsDePrueba prepara un ROOTFS_DIR como el del constructor llm.
func rootfsDePrueba(t *testing.T, dir string, gguf []byte) (rootfs, agent, cached string) {
	t.Helper()
	rootfs = filepath.Join(dir, "rootfs")
	for _, d := range []string{"opt/llama.cpp", "models", "etc/von"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := func(p, s string, m os.FileMode) {
		if err := os.WriteFile(filepath.Join(rootfs, p), []byte(s), m); err != nil {
			t.Fatal(err)
		}
		os.Chmod(filepath.Join(rootfs, p), m)
	}
	w("opt/llama.cpp/llama-server", "\x7fELF llama", 0o755)
	w("opt/llama.cpp/libggml.so.0.1", "lib", 0o755)
	if err := os.Symlink("libggml.so.0.1", filepath.Join(rootfs, "opt/llama.cpp/libggml.so.0")); err != nil {
		t.Fatal(err)
	}
	w("etc/von/run.sh", "#!/bin/sh\nexec llama-server\n", 0o755)
	w("etc/von/model.json", "{}\n", 0o644)
	cached = filepath.Join(dir, "cache.gguf")
	if err := os.WriteFile(cached, gguf, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(cached, filepath.Join(rootfs, "models", "m.gguf")); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(rootfs, "models", "m.gguf"), 0o644)
	agent = filepath.Join(dir, "kling-guest")
	if err := os.WriteFile(agent, []byte("\x7fELF agente"), 0o755); err != nil {
		t.Fatal(err)
	}
	return rootfs, agent, cached
}

// entrypointDelScript es el /entrypoint que escribiría 81-base-image.sh.
func entrypointDelScript(t *testing.T, service string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("sin bash")
	}
	cmd := exec.Command("bash", script81)
	cmd.Env = append(os.Environ(), "KLING_PRINT_ENTRYPOINT=1", "NAME=x", "AGENT=/nonexistent", "SERVICE="+service)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("81-base-image.sh en modo de imprimir el entrypoint: %v\n%s", err, out)
	}
	return string(out)
}

type entrada struct {
	modo     uint32
	uid, gid uint32
	datos    string // contenido o destino del enlace
}

func (e entrada) String() string {
	return fmt.Sprintf("%o %d:%d %q", e.modo, e.uid, e.gid, e.datos)
}

// arbolDe lee el /upper de una capa: ruta -> modo, dueño y contenido.
func arbolDe(t *testing.T, capa string) (map[string]entrada, []string) {
	t.Helper()
	f, err := os.Open(capa)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	root, err := ext4.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]entrada{}
	err = root.Lookup("/upper").Walk(func(p string, n *ext4.Node) error {
		e := entrada{modo: n.Mode, uid: n.UID, gid: n.GID}
		switch {
		case n.IsReg():
			b, err := n.ReadAll()
			if err != nil {
				return err
			}
			e.datos = string(b)
		case n.IsLink():
			e.datos = n.Target
		}
		got[p] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, root.Children()
}

// La capa que escribe Go tiene el mismo árbol que la del script: rutas, modos,
// dueños, contenido del entrypoint (el texto que imprime el propio script) y
// del resolv.conf, y lo que sube de la base con el copy-up.
func TestCapaLLMIgualQueScript(t *testing.T) {
	dir := t.TempDir()
	gguf := append(bytes.Repeat([]byte("pesos"), 3000), make([]byte, 3*ext4.BlockSize)...)
	rootfs, agent, cached := rootfsDePrueba(t, dir, gguf)
	out := filepath.Join(dir, "out")
	_, err := construirCapaLLM(capaLLM{Name: "qwen", BaseImg: baseDePrueba(t, dir), Agent: agent, Rootfs: rootfs,
		Service: "/etc/von/run.sh", Out: out, T: time.Unix(1_800_000_000, 0), Reflink: cached, Modelo: "/models/m.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	got, raiz := arbolDe(t, filepath.Join(out, "qwen.layer.ext4"))
	sort.Strings(raiz)
	if strings.Join(raiz, " ") != "lost+found upper" {
		t.Fatalf("raíz de la capa = %v; quería lost+found y upper (sin work)", raiz)
	}

	yo, grupo := uint32(os.Getuid()), uint32(os.Getgid())
	dirR := func(perm uint32) entrada { return entrada{modo: ext4.ModeDir | perm, uid: yo, gid: grupo} }
	want := map[string]entrada{
		// cp -a de ROOTFS_DIR/.: la raíz y sus directorios, con los atributos de ROOTFS_DIR.
		"/":                             dirR(0o755),
		"/opt":                          dirR(0o755),
		"/opt/llama.cpp":                dirR(0o755),
		"/opt/llama.cpp/llama-server":   {ext4.ModeReg | 0o755, yo, grupo, "\x7fELF llama"},
		"/opt/llama.cpp/libggml.so.0.1": {ext4.ModeReg | 0o755, yo, grupo, "lib"},
		"/opt/llama.cpp/libggml.so.0":   {ext4.ModeLink | 0o777, yo, grupo, "libggml.so.0.1"},
		"/models":                       dirR(0o755),
		"/models/m.gguf":                {ext4.ModeReg | 0o644, yo, grupo, string(gguf)},
		"/etc":                          dirR(0o755),
		"/etc/von":                      dirR(0o755),
		"/etc/von/run.sh":               {ext4.ModeReg | 0o755, yo, grupo, "#!/bin/sh\nexec llama-server\n"},
		"/etc/von/model.json":           {ext4.ModeReg | 0o644, yo, grupo, "{}\n"},
		// install -Dm755: los padres suben de la base tal cual.
		"/usr":                       {ext4.ModeDir | 0o755, 0, 0, ""},
		"/usr/local":                 {ext4.ModeDir | 0o2775, 0, 50, ""},
		"/usr/local/bin":             {ext4.ModeDir | 0o755, 0, 50, ""},
		"/usr/local/bin/kling-guest": {ext4.ModeReg | 0o755, 0, 0, "\x7fELF agente"},
		// El entrypoint del script y el resolv.conf de la base, truncado y reescrito.
		"/entrypoint":      {ext4.ModeReg | 0o755, 0, 0, entrypointDelScript(t, "/etc/von/run.sh")},
		"/etc/resolv.conf": {ext4.ModeReg | 0o640, 0, 7, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"},
	}
	for p, w := range want {
		g, ok := got[p]
		if !ok {
			t.Errorf("falta %s", p)
			continue
		}
		if g != w {
			t.Errorf("%s = %v; el script deja %v", p, g, w)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("sobra %s", p)
		}
	}
	if !strings.Contains(got["/entrypoint"].datos, "service exited with $?, restarting in 1s") {
		t.Fatalf("el entrypoint perdió la línea que cuenta el fabricante: %q", got["/entrypoint"].datos)
	}
}

// Un build que falla a mitad no deja *.layer.ext4: ni el de Go (se escribe a
// un temporal y se renombra al final) ni el del script (que antes truncaba y
// formateaba la capa en su ruta definitiva).
func TestCapaLLMCortadaNoDejaFichero(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root lee un fichero 0000")
		}
		dir := t.TempDir()
		rootfs, agent, _ := rootfsDePrueba(t, dir, []byte("pesos"))
		// Un fichero que no se puede leer: la escritura falla cuando ya ha
		// empezado.
		ilegible := filepath.Join(rootfs, "opt", "llama.cpp", "llama-server")
		os.Chmod(ilegible, 0)
		t.Cleanup(func() { os.Chmod(ilegible, 0o755) })
		out := filepath.Join(dir, "out")
		_, err := construirCapaLLM(capaLLM{Name: "qwen", BaseImg: baseDePrueba(t, dir), Agent: agent, Rootfs: rootfs,
			Service: "/etc/von/run.sh", Out: out, T: time.Now()})
		if err == nil {
			t.Fatal("construir con un fichero ilegible no falló")
		}
		if left, _ := filepath.Glob(filepath.Join(out, "*")); len(left) > 0 {
			t.Fatalf("quedó %v", left)
		}
		if left, _ := filepath.Glob(filepath.Join(out, ".*")); len(left) > 0 {
			t.Fatalf("quedó %v", left)
		}
	})
	t.Run("script", func(t *testing.T) {
		if _, err := exec.LookPath("bash"); err != nil {
			t.Skip("sin bash")
		}
		dir := t.TempDir()
		root := filepath.Join(dir, "root")
		out := filepath.Join(dir, "out")
		os.MkdirAll(filepath.Join(root, "images"), 0o755)
		os.WriteFile(filepath.Join(root, "images", "glibc.ext4"), []byte("base"), 0o644)
		agent := filepath.Join(dir, "kling-guest")
		os.WriteFile(agent, []byte("\x7fELF"), 0o755)
		// Falsos: root, un truncate que crea el fichero y un mkfs que falla.
		bin := filepath.Join(dir, "bin")
		os.MkdirAll(bin, 0o755)
		for name, body := range map[string]string{
			"id":        "echo 0\n",
			"truncate":  "for a in \"$@\"; do f=\"$a\"; done; : > \"$f\"\n",
			"mkfs.ext4": "exit 1\n",
		} {
			os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755)
		}
		cmd := exec.Command("bash", script81)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "KLING_ROOT="+root, "NAME=qwen",
			"BASE=glibc", "AGENT="+agent, "GROW=64", "KLING_OUT_DIR="+out)
		if b, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("el script siguió tras un mkfs fallido:\n%s", b)
		}
		var left []string
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && strings.Contains(filepath.Base(p), "layer.ext4") {
				left = append(left, p)
			}
			return nil
		})
		if len(left) > 0 {
			t.Fatalf("un build cortado dejó %v", left)
		}
	})
}

// La salida del servicio va solo a /var/log/service.log: la consola serie es
// lenta, y con tee cada línea de llama-server esperaba a la consola (ronda 9:
// primera respuesta 955 → 1032 ms, generación 32 → 29 tok/s). Cuando el
// servicio muere, el entrypoint vuelca a /dev/console las últimas 30 líneas
// del log, cada una con dorado.ServiceLogDumpPrefix, y justo después la línea
// `service exited with …` intacta, que es la que cuenta el fabricante sin
// exec. Igual en el script, en entrypointBase y en internal/imagen.
func TestServicioVuelcaALaConsolaAlMorir(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sin sh")
	}
	dir := t.TempDir()
	svc := filepath.Join(dir, "svc.sh")
	os.WriteFile(svc, []byte("#!/bin/sh\nfor i in $(seq 10 49); do echo linea-$i; done\nexit 3\n"), 0o755)
	lineaDe := func(ep string) string {
		for _, l := range strings.Split(ep, "\n") {
			if strings.Contains(l, "while :") {
				return l
			}
		}
		t.Fatalf("el entrypoint no arranca el servicio:\n%s", ep)
		return ""
	}
	base := lineaDe(entrypointBase(svc))
	if otra := lineaDe(imagen.Entrypoint(imagen.Marca{Constructor: "debian"}, nil, svc)); otra != base {
		t.Errorf("internal/imagen arranca el servicio distinto:\n%s\nfrente a\n%s", otra, base)
	}
	if _, err := exec.LookPath("bash"); err == nil {
		if otra := lineaDe(entrypointDelScript(t, svc)); otra != base {
			t.Errorf("81-base-image.sh arranca el servicio distinto:\n%s\nfrente a\n%s", otra, base)
		}
	}
	log := filepath.Join(dir, "service.log")
	consolaPath := filepath.Join(dir, "consola")
	// >> y no >: en el invitado es un dispositivo, aquí un fichero que cada
	// muerte truncaría.
	if !strings.Contains(base, ">/dev/console") {
		t.Fatalf("el entrypoint no vuelca a /dev/console: %s", base)
	}
	linea := strings.ReplaceAll(strings.ReplaceAll(base, "/var/log/service.log", log), ">/dev/console", ">>"+consolaPath)
	cmd := exec.Command("sh", "-c", linea+"\nsleep 1.5\n")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Un fichero y no un pipe: el bucle en segundo plano lo sigue teniendo
	// abierto, y Wait esperaría a que lo cerrase.
	sf, err := os.Create(filepath.Join(dir, "salida"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = sf, sf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	sf.Close()
	time.Sleep(100 * time.Millisecond)
	salida, _ := os.ReadFile(filepath.Join(dir, "salida"))
	consola, _ := os.ReadFile(consolaPath)
	fichero, _ := os.ReadFile(log)
	if len(salida) > 0 {
		t.Errorf("la salida del servicio no debe ir a la del entrypoint (la consola): %q", salida)
	}
	marca := dorado.ServiceExitedMark + "3, restarting in 1s"
	nFichero := strings.Count(string(fichero), marca)
	if nFichero == 0 || !strings.Contains(string(fichero), "linea-10\n") {
		t.Fatalf("service.log debe tener toda la salida y la marca: %q", fichero)
	}
	// En la consola: solo líneas del volcado y marcas, tantas marcas sin
	// prefijo como muertes (las copias del volcado llevan el prefijo).
	marcas := 0
	for _, l := range strings.Split(strings.TrimRight(string(consola), "\n"), "\n") {
		switch {
		case l == marca:
			marcas++
		case strings.HasPrefix(l, dorado.ServiceLogDumpPrefix):
		default:
			t.Errorf("línea de la consola sin prefijo ni marca: %q", l)
		}
	}
	if marcas != nFichero {
		t.Errorf("la consola tiene %d marcas y service.log %d:\n%s", marcas, nFichero, consola)
	}
	// La primera muerte: las últimas 30 líneas (20-49), no las de antes.
	c := string(consola)
	primera := c[:strings.Index(c, marca)]
	if !strings.Contains(primera, dorado.ServiceLogDumpPrefix+"linea-49\n") || !strings.Contains(primera, dorado.ServiceLogDumpPrefix+"linea-20\n") ||
		strings.Contains(primera, "linea-19") || strings.Contains(primera, dorado.ServiceExitedMark) {
		t.Errorf("el primer volcado debe ser linea-20…linea-49, sin marcas:\n%s", primera)
	}
}

// Lo que se clonaría con FICLONERANGE tiene que ser exactamente el modelo:
// cada tramo, leído de la imagen, son los mismos bytes que en el GGUF en ese
// desplazamiento, y entre todos cubren sus bloques enteros que no son huecos.
func TestBloquesDeMapeaElModelo(t *testing.T) {
	dir := t.TempDir()
	gguf := append(datosAleatorios(5*ext4.BlockSize+100), make([]byte, 4*ext4.BlockSize)...)
	gguf = append(gguf, datosAleatorios(3*ext4.BlockSize)...)
	rootfs, agent, _ := rootfsDePrueba(t, dir, gguf)
	out := filepath.Join(dir, "out")
	if _, err := construirCapaLLM(capaLLM{Name: "m", BaseImg: baseDePrueba(t, dir), Agent: agent, Rootfs: rootfs,
		Service: "/etc/von/run.sh", Out: out, T: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(out, "m.layer.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runs, err := bloquesDe(f, "/upper/models/m.gguf")
	if err != nil {
		t.Fatal(err)
	}
	cubiertos := int64(0)
	for _, r := range runs {
		b := make([]byte, r.n*ext4.BlockSize)
		if _, err := f.ReadAt(b, r.imagen*ext4.BlockSize); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, gguf[r.fichero*ext4.BlockSize:(r.fichero+r.n)*ext4.BlockSize]) {
			t.Fatalf("el tramo %+v no son los bytes del modelo", r)
		}
		cubiertos += r.n
	}
	// 5 bloques de datos + el que va a medias (100 bytes, no entero: fuera) +
	// 4 de ceros (hueco) + 3 de datos -> 8 enteros con datos... el de 100
	// bytes de datos más ceros sí es entero y con datos.
	if cubiertos != 9 {
		t.Fatalf("%d bloques cubiertos; quería 9 (los enteros que no son huecos)", cubiertos)
	}
}

// Tras construir, el GGUF de la caché se borra (la capa ya lo lleva y el
// dorado, en su mem.file): tres copias por modelo eran 3,4 GiB por GiB. Se
// queda con keep_cache, si lo convirtió kindling (no se puede volver a bajar)
// o si la capa comparte sus bloques (entonces no ocupa).
func TestKeepCache(t *testing.T) {
	const gib = int64(1 << 30)
	for _, c := range []struct {
		keep, convertido bool
		compartido       int64
		quedar           bool
	}{
		{false, false, 0, false},
		{true, false, 0, true},
		{false, true, 0, true},
		{false, false, gib - ext4.BlockSize, true},
		{false, false, gib / 2, false},
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "x.gguf")
		os.WriteFile(p, []byte("pesos"), 0o600)
		os.WriteFile(p+selloExt, []byte("{}"), 0o600)
		soltarCacheModelo(p, gib, c.keep, c.convertido, c.compartido)
		_, err := os.Stat(p)
		_, errSello := os.Stat(p + selloExt)
		if (err == nil) != c.quedar || (errSello == nil) != c.quedar {
			t.Errorf("%+v: el GGUF sigue = %v, el sello sigue = %v", c, err == nil, errSello == nil)
		}
	}
	// keep_cache viaja en el spec del modelo.
	if !keepCacheDe([]byte(`{"model":"qwen","keep_cache":true}`)) || keepCacheDe([]byte(`{"model":"qwen"}`)) || keepCacheDe(nil) {
		t.Fatal("keep_cache del spec mal leído")
	}
}
