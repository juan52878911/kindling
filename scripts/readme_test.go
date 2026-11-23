package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Las líneas de «Install» de los README son lo primero que teclea alguien: si
// una orden ya no existe, si `sudo kling` no encuentra el binario o si el pin
// de versión es de un kling que no sabe comprobar la firma sin minisign, el
// primer uso aborta. Este test las vigila contra el kling compilado del árbol.

// bloquesInstall devuelve las líneas de los bloques ```sh de la sección de
// instalación de un README (hasta el siguiente «## »).
func bloquesInstall(t *testing.T, readme, titulo string) [][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", readme))
	if err != nil {
		t.Fatal(err)
	}
	var bloques [][]string
	var actual []string
	dentro, enBloque := false, false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, "## "):
			dentro = strings.TrimSpace(strings.TrimPrefix(l, "## ")) == titulo
		case dentro && strings.HasPrefix(l, "```sh"):
			enBloque, actual = true, nil
		case dentro && enBloque && strings.HasPrefix(l, "```"):
			enBloque = false
			bloques = append(bloques, actual)
		case dentro && enBloque:
			actual = append(actual, l)
		}
	}
	if len(bloques) == 0 {
		t.Fatalf("%s: sin bloques sh en «%s»", readme, titulo)
	}
	return bloques
}

var secureDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// semverMenor dice si a < b (con prerelease: 1.0.0-rc.3 < 1.0.0).
func semverMenor(a, b string) bool {
	parse := func(v string) ([]int, []string) {
		v = strings.TrimPrefix(v, "v")
		core, pre, _ := strings.Cut(v, "-")
		var n []int
		for _, p := range strings.Split(core, ".") {
			i, _ := strconv.Atoi(p)
			n = append(n, i)
		}
		if pre == "" {
			return n, nil
		}
		return n, strings.Split(pre, ".")
	}
	na, pa := parse(a)
	nb, pb := parse(b)
	for i := 0; i < len(na) && i < len(nb); i++ {
		if na[i] != nb[i] {
			return na[i] < nb[i]
		}
	}
	switch {
	case pa == nil:
		return false
	case pb == nil:
		return true
	}
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, ex := strconv.Atoi(pa[i])
		y, ey := strconv.Atoi(pb[i])
		if ex == nil && ey == nil && x != y {
			return x < y
		}
		if (ex != nil || ey != nil) && pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

func primerUsoDesde(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^PRIMER_USO_DESDE="(v[^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("install.sh no define PRIMER_USO_DESDE")
	}
	return string(m[1])
}

func TestReadmeInstallOrdenesYPin(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("sin go")
	}
	bin := filepath.Join(t.TempDir(), "kling")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/kling").CombinedOutput(); err != nil {
		t.Skipf("cannot build kling here: %v\n%s", err, out)
	}
	desde := primerUsoDesde(t)
	cache := map[string]string{}
	ayuda := func(args ...string) string {
		k := strings.Join(args, " ")
		if s, ok := cache[k]; ok {
			return s
		}
		cmd := exec.Command(bin, append(append([]string{}, args...), "-h")...)
		// Sin esto -h imprime la ayuda del árbol y no los flags del comando.
		cmd.Env = append(os.Environ(), "KLING_FLAG_HELP=1", "KLING_HOST=unix:///nonexistent/kling.sock", "HOME="+t.TempDir())
		out, _ := cmd.CombinedOutput()
		cache[k] = string(out)
		return cache[k]
	}

	for _, readme := range []struct{ fichero, titulo string }{{"README.md", "Install"}, {"README.es.md", "Instalación"}} {
		for _, bloque := range bloquesInstall(t, readme.fichero, readme.titulo) {
			instalaConSudo := false
			for _, l := range bloque {
				if i := strings.Index(l, " #"); i >= 0 {
					l = l[:i]
				}
				l = strings.TrimSpace(l)
				if strings.HasPrefix(l, "curl ") {
					// (c) el pin.
					if m := regexp.MustCompile(`KLING_VERSION=(v\S+)`).FindStringSubmatch(l); m != nil && semverMenor(m[1], desde) {
						t.Errorf("%s: fija %s, menor que PRIMER_USO_DESDE %s (esa release no comprueba la firma sin minisign)", readme.fichero, m[1], desde)
					}
					if regexp.MustCompile(`\|\s*sudo\b`).MatchString(l) {
						instalaConSudo = true
						for _, p := range regexp.MustCompile(`(?:KLING_PREFIX=|--prefix[ =])(\S+)`).FindAllStringSubmatch(l, -1) {
							if !contiene(secureDirs, p[1]) {
								t.Errorf("%s: instala en %s, fuera del secure_path de sudo: %s", readme.fichero, p[1], l)
							}
						}
					}
					continue
				}
				// (b) sudo kling.
				if strings.HasPrefix(l, "sudo kling") && !instalaConSudo {
					t.Errorf("%s: `%s` sin haber instalado kling con `| sudo … sh`: sudo no encuentra un kling de ~/.local/bin", readme.fichero, l)
				}
				// (a) kling <cmd> <flag>.
				f := strings.Fields(strings.TrimPrefix(l, "sudo "))
				if len(f) < 2 || f[0] != "kling" {
					continue
				}
				var cmd, flags []string
				for i, a := range f[1:] {
					if a == "--" {
						break
					}
					if strings.HasPrefix(a, "-") {
						flags = append(flags, strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-"))
					} else if i < 2 && len(flags) == 0 {
						cmd = append(cmd, a)
					}
				}
				if len(cmd) == 0 {
					continue
				}
				if h := ayuda(cmd[0]); strings.Contains(h, "unknown command") {
					t.Errorf("%s: `%s`: kling no tiene la orden %q", readme.fichero, l, cmd[0])
					continue
				}
				for _, fl := range flags {
					re := regexp.MustCompile(`(?m)^\s+-` + regexp.QuoteMeta(fl) + `\b`)
					ok := re.MatchString(ayuda(cmd[0]))
					if !ok && len(cmd) > 1 {
						ok = re.MatchString(ayuda(cmd...))
					}
					if !ok {
						t.Errorf("%s: `%s`: `kling %s` no tiene el flag -%s", readme.fichero, l, strings.Join(cmd, " "), fl)
					}
				}
			}
		}
	}
}

func contiene(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// El bloque de Linux es el mismo en los dos README: curl con sudo, sudo kling
// up -install, kling try.
func TestReadmeLinuxTresLineas(t *testing.T) {
	for _, r := range []struct{ f, t string }{{"README.md", "Install"}, {"README.es.md", "Instalación"}} {
		b := bloquesInstall(t, r.f, r.t)[0]
		var l []string
		for _, x := range b {
			if x = strings.TrimSpace(x); x != "" {
				l = append(l, x)
			}
		}
		if len(l) != 3 || !strings.HasPrefix(l[0], "curl ") || !regexp.MustCompile(`\|\s*sudo\b.*\bsh$`).MatchString(l[0]) ||
			!strings.HasPrefix(l[1], "sudo kling up -install") || !strings.HasPrefix(l[2], "kling try") {
			t.Errorf("%s: el bloque de Linux es %q", r.f, l)
		}
	}
}

func TestSemverMenor(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1.0.0-rc.2", "v1.0.0-rc.3", true}, {"v1.0.0-rc.3", "v1.0.0-rc.3", false},
		{"v1.0.0-rc.10", "v1.0.0-rc.9", false}, {"v1.0.0", "v1.0.0-rc.3", false},
		{"v1.0.0-rc.3", "v1.0.0", true}, {"v0.9.9", "v1.0.0-rc.1", true},
	} {
		if got := semverMenor(c.a, c.b); got != c.want {
			t.Errorf("semverMenor(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
