package guest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/pkg/api"
)

// fakeInit es un sistema falso para el init: apunta cada llamada y guarda los
// ficheros en un mapa. No sigue el pivot_root: las rutas son las que pide el
// init, que es lo que se comprueba.
type fakeInit struct {
	calls  []string
	files  map[string]string
	links  map[string]string
	fail   map[string]error // por llamada apuntada: "mount /dev/vdb /overlay ext4 0 "
	exec   []string
	execEv []string
}

func newFakeInit() *fakeInit {
	return &fakeInit{files: map[string]string{}, links: map[string]string{}, fail: map[string]error{}}
}

func (f *fakeInit) call(s string) error {
	f.calls = append(f.calls, s)
	return f.fail[s]
}

func (f *fakeInit) Mount(src, target, fstype string, flags uintptr, data string) error {
	return f.call(strings.TrimSpace(fmt.Sprintf("mount %s %s %s %#x %s", src, target, fstype, flags, data)))
}
func (f *fakeInit) Unmount(t string) error                 { return f.call("umount " + t) }
func (f *fakeInit) MkdirAll(p string, _ fs.FileMode) error { return f.call("mkdir " + p) }
func (f *fakeInit) PivotRoot(n, o string) error            { return f.call("pivot_root " + n + " " + o) }
func (f *fakeInit) Chdir(d string) error                   { return f.call("cd " + d) }

func (f *fakeInit) ReadFile(p string) ([]byte, error) {
	if b, ok := f.files[p]; ok {
		return []byte(b), nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeInit) WriteFile(p string, b []byte) error {
	f.files[p] = string(b)
	return f.call("write " + p)
}

func (f *fakeInit) AppendFile(p string, b []byte) error {
	f.files[p] += string(b)
	return f.call("append " + p)
}

func (f *fakeInit) Stat(p string) error {
	if _, ok := f.files[p]; ok {
		return nil
	}
	return fs.ErrNotExist
}

func (f *fakeInit) Lstat(p string) error {
	if _, ok := f.links[p]; ok {
		return nil
	}
	return f.Stat(p)
}

func (f *fakeInit) Symlink(t, p string) error {
	f.links[p] = t
	return f.call("ln " + t + " " + p)
}

func (f *fakeInit) Exec(p string, argv, env []string) error {
	f.exec, f.execEv = argv, env
	return f.call("exec " + p)
}

// El plan entero, en orden: lo mismo que hace minimal-init.sh para una imagen
// de Docker (sin tmpfs en /tmp y /run), y con la capa de servicio.
func TestInitPlan(t *testing.T) {
	for _, c := range []struct {
		name    string
		cmdline string
		oci     bool
		want    string
	}{
		{"oci", "console=ttyS0 root=/dev/vda ro\n", true, `
mount proc /proc proc 0x0
mount sysfs /sys sysfs 0x0
mount devtmpfs /dev devtmpfs 0x0
mkdir /overlay
mount /dev/vdb /overlay ext4 0x0
mkdir /overlay/upper
mkdir /overlay/work
mkdir /overlay/merged
mount overlay /overlay/merged overlay 0x0 lowerdir=/,upperdir=/overlay/upper,workdir=/overlay/work
mkdir /overlay/merged/rom
cd /overlay/merged
pivot_root . rom
cd /
mkdir /proc
mkdir /sys
mkdir /dev
mkdir /tmp
mkdir /run
mount proc /proc proc 0x0
mount sysfs /sys sysfs 0x0
mount devtmpfs /dev devtmpfs 0x0
umount /rom/proc
umount /rom/sys
umount /rom/dev
ln /proc/self/fd /dev/fd
ln /proc/self/fd/0 /dev/stdin
ln /proc/self/fd/1 /dev/stdout
ln /proc/self/fd/2 /dev/stderr
mkdir /dev/shm
mount tmpfs /dev/shm tmpfs 0x6 mode=1777
write /proc/sys/kernel/hostname
append /etc/hosts
exec /usr/local/bin/kling-guest`},
		{"base with layer", "console=ttyS0 kling.layerx=/dev/vdz kling.layer=/dev/vdd quiet", false, `
mount proc /proc proc 0x0
mount sysfs /sys sysfs 0x0
mount devtmpfs /dev devtmpfs 0x0
mkdir /overlay
mount /dev/vdb /overlay ext4 0x0
mkdir /overlay/upper
mkdir /overlay/work
mkdir /overlay/merged
mkdir /overlay/svc
mount /dev/vdd /overlay/svc ext4 0x1
mount overlay /overlay/merged overlay 0x0 lowerdir=/overlay/svc/upper:/,upperdir=/overlay/upper,workdir=/overlay/work
mkdir /overlay/merged/rom
cd /overlay/merged
pivot_root . rom
cd /
mkdir /proc
mkdir /sys
mkdir /dev
mkdir /tmp
mkdir /run
mount proc /proc proc 0x0
mount sysfs /sys sysfs 0x0
mount devtmpfs /dev devtmpfs 0x0
mount tmpfs /tmp tmpfs 0x0
mount tmpfs /run tmpfs 0x0
umount /rom/proc
umount /rom/sys
umount /rom/dev
ln /proc/self/fd /dev/fd
ln /proc/self/fd/0 /dev/stdin
ln /proc/self/fd/1 /dev/stdout
ln /proc/self/fd/2 /dev/stderr
mkdir /dev/shm
mount tmpfs /dev/shm tmpfs 0x6 mode=1777
write /proc/sys/kernel/hostname
append /etc/hosts
exec /usr/local/bin/kling-guest`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeInit()
			f.files["/proc/cmdline"] = c.cmdline
			f.files["/proc/sys/kernel/hostname"] = "(none)\n"
			if c.oci {
				f.files[ociMarker] = "{}"
			}
			err := runInit(f, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := "\n" + strings.Join(f.calls, "\n"); got != c.want {
				t.Fatalf("plan:%s\nwant:%s", got, c.want)
			}
			if strings.Join(f.exec, " ") != "/usr/local/bin/kling-guest -listen :8080" {
				t.Fatalf("exec %q", f.exec)
			}
			if f.files["/proc/sys/kernel/hostname"] != "kindling\n" ||
				f.files["/etc/hosts"] != "127.0.0.1\tlocalhost\n127.0.1.1\tkindling\n" {
				t.Fatalf("hostname %q, hosts %q", f.files["/proc/sys/kernel/hostname"], f.files["/etc/hosts"])
			}
		})
	}
}

// OVERLAY_DEV (del kernel) cambia el disco del overlay; lo que ya existe en
// /dev no se pisa; y lo que el script no toleraba para el arranque, sin exec.
func TestInitOverlayDevAndFailures(t *testing.T) {
	f := newFakeInit()
	f.files["/dev/fd"] = ""
	f.links["/dev/stdin"] = "/somewhere"
	if err := runInit(f, []string{"OVERLAY_DEV=/dev/vdq"}); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(f.calls, "\n")
	if !strings.Contains(plan, "mount /dev/vdq /overlay ext4") || strings.Contains(plan, "/dev/vdb") {
		t.Fatalf("OVERLAY_DEV not used:\n%s", plan)
	}
	if strings.Contains(plan, "ln /proc/self/fd /dev/fd") || strings.Contains(plan, "/dev/stdin") {
		t.Fatalf("existing /dev entries replaced:\n%s", plan)
	}

	for _, fatal := range []string{
		"mount proc /proc proc 0x0",
		"mount /dev/vdb /overlay ext4 0x0",
		"mount /dev/vdc /overlay/svc ext4 0x1",
		"pivot_root . rom",
		"exec /usr/local/bin/kling-guest",
	} {
		f := newFakeInit()
		f.files["/proc/cmdline"] = "kling.layer=/dev/vdc"
		f.fail[fatal] = errors.New("boom")
		err := runInit(f, nil)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("%q failing: %v", fatal, err)
		}
		if fatal != "exec /usr/local/bin/kling-guest" && f.exec != nil {
			t.Fatalf("%q failing, but the agent was started", fatal)
		}
	}
	// Lo que el script toleraba no para el arranque.
	for _, soft := range []string{
		"mount devtmpfs /dev devtmpfs 0x0", "umount /rom/dev", "mount tmpfs /dev/shm tmpfs 0x6 mode=1777",
		"append /etc/hosts", "write /proc/sys/kernel/hostname",
	} {
		f := newFakeInit()
		f.fail[soft] = errors.New("boom")
		if err := runInit(f, nil); err != nil || f.exec == nil {
			t.Fatalf("%q failing stopped the boot: %v", soft, err)
		}
	}
}

// Los casos de TestMinimalInitHostsSinGrep (scripts/), con el init en Go.
func TestInitHosts(t *testing.T) {
	for _, c := range []struct{ antes, despues string }{
		{"", "127.0.0.1\tlocalhost\n127.0.1.1\tmaquina\n"},
		{"127.0.0.1 localhost.localdomain localhost # local\n127.0.1.1\tmaquina\n", ""},
		{"::1 localhost\n# 127.0.0.1 localhost maquina\n10.0.0.1 maquina.lan *", "\n127.0.0.1\tlocalhost\n127.0.1.1\tmaquina\n"},
		{"127.0.0.1\tlocalhost\n10.0.0.1 maquina", ""},
		{"  127.0.0.1 localhost#comentario\n\t10.0.0.1\tx maquina#y\n", ""},
		{"127.0.0.1 localhost\n", "127.0.1.1\tmaquina\n"},
	} {
		f := newFakeInit()
		if c.antes != "" {
			f.files["/etc/hosts"] = c.antes
		}
		initHosts(f, "maquina")
		if got := f.files["/etc/hosts"]; got != c.antes+c.despues {
			t.Errorf("con %q:\n%q\nquería\n%q", c.antes, got, c.antes+c.despues)
		}
		// Idempotente.
		before := f.files["/etc/hosts"]
		initHosts(f, "maquina")
		if f.files["/etc/hosts"] != before {
			t.Errorf("con %q, la segunda vuelta: %q", c.antes, f.files["/etc/hosts"])
		}
	}
}

func TestInitHostname(t *testing.T) {
	for in, want := range map[string]string{
		"":             "kindling",
		"(none)\n":     "kindling",
		"172.16.0.2\n": "kindling",
		"web-1\n":      "web-1",
		" pg \n":       "pg",
		"10.0.0.1a\n":  "10.0.0.1a",
	} {
		f := newFakeInit()
		f.files["/proc/sys/kernel/hostname"] = in
		if got := initHostname(f); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
		if wrote := f.files["/proc/sys/kernel/hostname"] != in; wrote != (want == "kindling") {
			t.Errorf("%q: wrote the hostname = %v", in, wrote)
		}
	}
}

// El entorno del agente: el del kernel, PATH y HOME del /entrypoint, y encima
// /etc/kling/env tal como lo escribe imagen.EnvFile, comillas incluidas. Una
// línea que no se entiende se salta sin que su valor llegue al registro.
func TestInitEnv(t *testing.T) {
	vars := []string{"PATH=/app/bin:/bin", "QUOTE=it's \"x\" $HOME `y`", "EMPTY=", "SP=a  b\tc", "EQ=a=b",
		"MULTI=a\nb'\n\nc # no\n", "AFTER=1"}
	f := newFakeInit()
	f.files[guestEnvPath] = imagen.EnvFile(vars) + "export BAD=$(rm -rf /)\nnot a line\nexport 1X='v'\n"
	env := initEnv(f, []string{"TERM=linux", "HOME=/"})
	want := []string{"TERM=linux", "HOME=/root", "PATH=/app/bin:/bin", "QUOTE=it's \"x\" $HOME `y`", "EMPTY=", "SP=a  b\tc", "EQ=a=b",
		"MULTI=a\nb'\n\nc # no\n", "AFTER=1"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env:\n%q\nwant\n%q", env, want)
	}
	// Sin fichero: PATH y HOME.
	env = initEnv(newFakeInit(), nil)
	if strings.Join(env, " ") != "PATH="+initPATH+" HOME=/root" {
		t.Fatalf("env without a file: %q", env)
	}
	if _, bad := parseEnvFile("export A='x\nexport B='y'\n"); len(bad) != 1 || bad[0] != 1 {
		t.Fatalf("bad lines %v", bad)
	}
	// Un valor multilínea (un ENV de la imagen con saltos), como lo carga sh:
	// la variable entera y lo que sigue en su sitio; un comentario con una
	// comilla no abre nada; el número de línea de lo malo es donde empieza.
	vars2, bad := parseEnvFile("# it's\nexport A='x\ny'\nexport B=b\\'c\nmal\nexport C='z\n")
	if strings.Join(vars2, "|") != "A=x\ny|B=b'c" || len(bad) != 2 || bad[0] != 5 || bad[1] != 6 {
		t.Fatalf("multiline: %q, bad lines %v", vars2, bad)
	}
}

// La marca del init se encuentra en cualquier sitio del binario, también
// partida entre dos lecturas, y un binario sin ella (un agente anterior al
// init en Go) no la tiene aunque lleve la cadena de kling.layer suelta.
func TestHasInitMarker(t *testing.T) {
	relleno := strings.Repeat("\x7fELF\x00", 20000) // más de un trozo de 64 KiB
	for _, corte := range []int{0, 1, len(InitMarker) / 2, len(InitMarker) - 1} {
		for _, pos := range []int{0, 64<<10 - corte, len(relleno)} {
			bin := relleno[:pos] + InitMarker + relleno[pos:]
			for nombre, r := range map[string]io.Reader{
				"entero":       strings.NewReader(bin),
				"byte a byte":  iotest.OneByteReader(strings.NewReader(bin)),
				"medio trozos": iotest.HalfReader(strings.NewReader(bin)),
			} {
				if ok, err := HasInitMarker(r); !ok || err != nil {
					t.Fatalf("pos %d, %s: %v %v", pos, nombre, ok, err)
				}
			}
		}
	}
	viejo := relleno + api.LayerBootParam + "library" + InitMarker[:len(InitMarker)-1]
	if ok, err := HasInitMarker(iotest.HalfReader(strings.NewReader(viejo))); ok || err != nil {
		t.Fatalf("an agent without the marker: %v %v", ok, err)
	}
	if _, err := HasInitMarker(iotest.ErrReader(io.ErrUnexpectedEOF)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error: %v", err)
	}
}
