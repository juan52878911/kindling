package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// trozo es la parte de minimal-init.sh entre la línea que empieza por desde
// (incluida) y la primera, después, que empieza por hasta (incluida).
func trozo(t *testing.T, desde, hasta string) string {
	t.Helper()
	i := strings.Index(MinimalInit, "\n"+desde)
	if i < 0 {
		t.Fatalf("minimal-init.sh sin %q", desde)
	}
	j := strings.Index(MinimalInit[i+1:], "\n"+hasta)
	if j < 0 {
		t.Fatalf("minimal-init.sh sin %q tras %q", hasta, desde)
	}
	resto := MinimalInit[i+1+j+1:]
	return MinimalInit[i+1:i+1+j+1] + resto[:strings.Index(resto, "\n")+1]
}

// shells son los sh con que se prueba: el del sistema y, si están, dash
// (Debian) y el ash de busybox (Alpine), que son los de las bases y las
// imágenes de Docker.
func shells(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	if p, err := exec.LookPath("sh"); err == nil {
		out = append(out, []string{p})
	}
	if p, err := exec.LookPath("dash"); err == nil {
		out = append(out, []string{p})
	}
	if p, err := exec.LookPath("busybox"); err == nil {
		out = append(out, []string{p, "sh"})
	}
	if len(out) == 0 {
		t.Skip("sin sh")
	}
	return out
}

// shSinPATH corre script con sh y PATH vacío: en una imagen de Docker el init
// solo cuenta con sh, mount, pivot_root, mkdir y ln (ociInitTools), así que
// estos trozos no pueden llamar a nada que no sea un builtin.
func shSinPATH(t *testing.T, sh []string, script string) string {
	t.Helper()
	cmd := exec.Command(sh[0], append(sh[1:], "-c", "set -e\n"+script)...)
	cmd.Env = []string{"PATH=/nonexistent"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", sh, err, out)
	}
	return string(out)
}

func TestMinimalInitCmdlineSinCat(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	os.WriteFile(cmdline, []byte("console=ttyS0 reboot=k kling.layer=/dev/vdc * quiet\n"), 0o644)
	s := trozo(t, `LAYER_DEV=""`, "set +f")
	s = strings.ReplaceAll(s, "/proc/cmdline", cmdline)
	for _, sh := range shells(t) {
		if got := shSinPATH(t, sh, s+`echo "[$LAYER_DEV]"`); got != "[/dev/vdc]\n" {
			t.Fatalf("%v, kling.layer: %q", sh, got)
		}
	}
}

func TestMinimalInitHostsSinGrep(t *testing.T) {
	hosts := filepath.Join(t.TempDir(), "hosts")
	s := strings.ReplaceAll(trozo(t, "{\n  HAS_LH", "} 2>/dev/null || true"), "/etc/hosts", hosts)
	for _, sh := range shells(t) {
		hostsCasos(t, sh, s, hosts)
	}
}

func hostsCasos(t *testing.T, sh []string, s, hosts string) {
	for _, c := range []struct{ antes, despues string }{
		// Sin fichero: los dos.
		{"", "127.0.0.1\tlocalhost\n127.0.1.1\tmaquina\n"},
		// Ya están (con tabuladores, alias y comentarios): nada.
		{"127.0.0.1 localhost.localdomain localhost # local\n127.0.1.1\tmaquina\n", ""},
		// "localhost" en otra dirección o en un comentario, y el nombre como
		// parte de otro, no cuentan. Una línea sin \n final se lee igual.
		{"::1 localhost\n# 127.0.0.1 localhost maquina\n10.0.0.1 maquina.lan *", "\n127.0.0.1\tlocalhost\n127.0.1.1\tmaquina\n"},
		{"127.0.0.1\tlocalhost\n10.0.0.1 maquina", ""},
	} {
		os.Remove(hosts)
		if c.antes != "" {
			os.WriteFile(hosts, []byte(c.antes), 0o644)
		}
		shSinPATH(t, sh, "HN=maquina\n"+s)
		b, _ := os.ReadFile(hosts)
		if want := c.antes + c.despues; string(b) != want {
			t.Errorf("%v con %q:\n%q\nquería\n%q", sh, c.antes, b, want)
		}
		// Idempotente: una segunda vuelta no añade nada.
		shSinPATH(t, sh, "HN=maquina\n"+s)
		if b2, _ := os.ReadFile(hosts); string(b2) != string(b) {
			t.Errorf("%v con %q, la segunda vuelta: %q", sh, c.antes, b2)
		}
	}
}
