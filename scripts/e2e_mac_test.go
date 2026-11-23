package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Los pendientes de la ronda 6 en los e2e (H-19): los skip que no cuentan, el
// árbol con ficheros sin añadir, el bidle que da por acabado lo que no acabó y
// la release de partida de 95 que no queda en el resultado. Se prueban los
// trozos de los scripts, no las pruebas enteras (necesitan microVMs).

// tramo devuelve las líneas de s desde la primera que empieza por desde hasta
// la primera que empieza por hasta (incluida); hasta vacío: hasta el final.
func tramo(t *testing.T, s, desde, hasta string) string {
	t.Helper()
	ls := strings.Split(s, "\n")
	i := -1
	for n, l := range ls {
		if strings.HasPrefix(l, desde) {
			i = n
			break
		}
	}
	if i < 0 {
		t.Fatalf("sin una línea que empiece por %q", desde)
	}
	if hasta == "" {
		return strings.Join(ls[i:], "\n")
	}
	for n := i; n < len(ls); n++ {
		if strings.HasPrefix(ls[n], hasta) {
			return strings.Join(ls[i:n+1], "\n")
		}
	}
	t.Fatalf("sin una línea que empiece por %q tras %q", hasta, desde)
	return ""
}

func leer(t *testing.T, f string) string {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// D10: 92 cuenta los skip y los dice en el resumen; con KLING_E2E_STRICT=1
// (el camino de la release) un skip hace fallar la pasada: sin psql, sin git o
// sin golden no da el mismo sello que una completa.
func TestE2EMac92CuentaLosSkip(t *testing.T) {
	s := leer(t, "92-e2e-mac.sh")
	ayudas := tramo(t, s, "pass=0; fail=0", "info()")
	resumen := tramo(t, s, `printf "\n\033[1m%d ok`, "")
	correr := func(strict string) (string, bool) {
		cmd := exec.Command("bash", "-c", ayudas+"\nok uno\nskip \"sin psql\"\n"+resumen)
		cmd.Env = append(os.Environ(), "KLING_E2E_STRICT="+strict)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}
	out, ok := correr("")
	if !ok {
		t.Errorf("sin KLING_E2E_STRICT un skip hizo fallar la pasada:\n%s", out)
	}
	if !strings.Contains(out, "1 skipped") {
		t.Errorf("el resumen no cuenta el skip:\n%s", out)
	}
	if out, ok := correr("1"); ok {
		t.Errorf("con KLING_E2E_STRICT=1 un skip salió con 0:\n%s", out)
	}
	// Ningún skip a mano fuera de las ayudas: todos pasan por el contador
	// (o por opcional, lo que el camino de la release no da y no cuenta).
	for n, l := range strings.Split(s, "\n") {
		if strings.Contains(l, `33mskip`) && !regexp.MustCompile(`^(skip|opcional)\(\)`).MatchString(l) {
			t.Errorf("92-e2e-mac.sh:%d: skip sin contar: %s", n+1, strings.TrimSpace(l))
		}
		if strings.Contains(l, `info "skipped`) {
			t.Errorf("92-e2e-mac.sh:%d: skip como info, sin contar: %s", n+1, strings.TrimSpace(l))
		}
	}
	// Y el camino de la release lo pide.
	if !strings.Contains(leer(t, "e2e-mac-release.sh"), "KLING_E2E_STRICT=1") {
		t.Error("e2e-mac-release.sh no corre 92 con KLING_E2E_STRICT=1")
	}
	if !regexp.MustCompile(`(?m)^\s+KLING_E2E_STRICT:\s*'1'`).MatchString(leer(t, "../.github/workflows/e2e-mac.yml")) {
		t.Error("e2e-mac.yml no corre 92 con KLING_E2E_STRICT: '1'")
	}
}

// D11: un fichero sin añadir (un .go que entra en go build) también ensucia el
// árbol: el sello no puede publicarse sobre un commit que no es lo probado. Lo
// que .gitignore ignora no cuenta.
func TestE2EMacReleaseSucioConFicheroSinAñadir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("sin git")
	}
	linea := tramo(t, leer(t, "e2e-mac-release.sh"), "SUCIO=", "SUCIO=")
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	escribir(t, filepath.Join(repo, ".gitignore"), "ignorado.log\n", 0o644)
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "x")
	sucio := func() string {
		cmd := exec.Command("bash", "-c", "TOP="+repo+"\n"+linea+"\nprintf '%s' \"$SUCIO\"")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return string(out)
	}
	if s := sucio(); s != "" {
		t.Fatalf("árbol limpio dado por sucio: %q", s)
	}
	escribir(t, filepath.Join(repo, "ignorado.log"), "x", 0o644)
	if s := sucio(); s != "" {
		t.Errorf("un fichero ignorado ensució el árbol: %q", s)
	}
	escribir(t, filepath.Join(repo, "nuevo.go"), "package x\n", 0o644)
	if s := sucio(); s != "1" {
		t.Errorf("un .go sin añadir no ensució el árbol: %q", s)
	}
}

// D12 en 92: bidle espera solo al settle de la prueba (el kling-db de su
// DBSTAGE), no a cualquiera del sistema, y al agotar la espera lo dice con
// bad y devuelve 1 en vez de darlo por acabado.
func TestE2EMac92BidleAcotadoYFallaAlAgotar(t *testing.T) {
	m := regexp.MustCompile(`(?m)^\s*(bidle\(\) \{.*\})\s*$`).FindStringSubmatch(leer(t, "92-e2e-mac.sh"))
	if m == nil {
		t.Fatal("92-e2e-mac.sh sin bidle() de una línea")
	}
	raiz := t.TempDir()
	falsos := filepath.Join(raiz, "bin")
	if err := os.MkdirAll(falsos, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(raiz, "pgrep.log")
	// pgrep falso: apunta sus argumentos y sale con $PGREP_RC; seq corto y
	// sleep que no duerme, para no esperar 120 s.
	escribir(t, filepath.Join(falsos, "pgrep"), "#!/bin/sh\necho \"$*\" >> \""+log+"\"\nexit ${PGREP_RC:-1}\n", 0o755)
	escribir(t, filepath.Join(falsos, "seq"), "#!/bin/sh\necho 1; echo 2\n", 0o755)
	escribir(t, filepath.Join(falsos, "sleep"), "#!/bin/sh\nexit 0\n", 0o755)
	correr := func(rc string) (string, bool) {
		guion := "bad() { echo \"BAD $*\"; }\nDBSTAGE=" + raiz + "/kling-e2e-db.Ab12Cd\nBR=$DBSTAGE/repo\n" + m[1] + "\nbidle\n"
		cmd := exec.Command("bash", "-c", guion)
		cmd.Env = append(os.Environ(), "PATH="+falsos+string(os.PathListSeparator)+os.Getenv("PATH"), "PGREP_RC="+rc)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}
	if out, ok := correr("1"); !ok || strings.Contains(out, "BAD") {
		t.Errorf("sin settle vivo bidle no salió limpio (ok=%v):\n%s", ok, out)
	}
	out, ok := correr("0")
	if ok {
		t.Errorf("con el settle vivo hasta el final bidle salió con 0:\n%s", out)
	}
	if !strings.Contains(out, "BAD") {
		t.Errorf("al agotar la espera bidle no lo dijo con bad:\n%s", out)
	}
	if args := leer(t, log); !strings.Contains(args, "kling-e2e-db.Ab12Cd") {
		t.Errorf("pgrep no se acota a la prueba (su DBSTAGE): %q", args)
	}
}

// D12 en 90 (Linux): bidle mira solo los settle cuyo directorio de trabajo es
// el repo de la prueba ($BR), y al agotar la espera falla. Con procesos de
// verdad, porque lo que distingue es /proc/<pid>/cwd.
func TestE2E90BidleAcotadoYFallaAlAgotar(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("90-e2e.sh es de Linux: bidle lee /proc/<pid>/cwd")
	}
	m := regexp.MustCompile(`(?m)^\s*(bidle\(\) \{.*\})\s*$`).FindStringSubmatch(leer(t, "90-e2e.sh"))
	if m == nil {
		t.Fatal("90-e2e.sh sin bidle() de una línea")
	}
	raiz := t.TempDir()
	br := filepath.Join(raiz, "repo")
	otro := filepath.Join(raiz, "otro")
	falsos := filepath.Join(raiz, "bin")
	for _, d := range []string{br, otro, falsos} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	escribir(t, filepath.Join(falsos, "seq"), "#!/bin/sh\necho 1; echo 2\n", 0o755)
	// Un proceso con «kling-db branch -settle» en su línea de órdenes, en dir.
	settle := func(dir string) *exec.Cmd {
		c := exec.Command("bash", "-c", "sleep 30; :", "kling-db", "branch", "-settle")
		c.Dir = dir
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
		time.Sleep(200 * time.Millisecond)
		return c
	}
	correr := func() (string, bool) {
		guion := "bad() { echo \"BAD $*\"; }\nBR=" + br + "\n" + m[1] + "\nbidle\n"
		cmd := exec.Command("bash", "-c", guion)
		cmd.Env = append(os.Environ(), "PATH="+falsos+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}
	settle(otro)
	if out, ok := correr(); !ok || strings.Contains(out, "BAD") {
		t.Errorf("el settle de otro repo hizo esperar a bidle (ok=%v):\n%s", ok, out)
	}
	settle(br)
	out, ok := correr()
	if ok || !strings.Contains(out, "BAD") {
		t.Errorf("con el settle de la prueba vivo bidle no falló (ok=%v):\n%s", ok, out)
	}
}

// D15: el resultado dice de qué release partió 95 (y si fue la fija porque el
// API no contestó): en la salida de 95 y en la descripción del status.
func TestE2EMacReleaseStatusDiceLaReleaseDePartida(t *testing.T) {
	out, ok, gh, _ := correrVeredicto(t, 0, 0)
	if !ok || !strings.Contains(gh, "state=success") {
		t.Fatalf("con 92 y 95 en verde no publicó success (ok=%v):\n%s\n%s", ok, gh, out)
	}
	if !strings.Contains(gh, "from v9.9.9") {
		t.Errorf("la descripción del status no dice la release de partida:\n%s", gh)
	}
	if !strings.Contains(out, "from v9.9.9") {
		t.Errorf("el resultado no dice la release de partida:\n%s", out)
	}
	if !strings.Contains(leer(t, "95-e2e-upgrade-mac.sh"), `echo "upgrade e2e (mac) from $OLD_TAG`) {
		t.Error("95-e2e-upgrade-mac.sh no dice en su resumen de qué release partió")
	}
}
