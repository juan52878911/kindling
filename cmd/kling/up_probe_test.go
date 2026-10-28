package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Las variables del sondeo remoto son datos: una raíz, un usuario o una unidad
// con comillas, `;` o `$(...)` no ejecutan nada en el host remoto y llegan al
// guion tal cual.
func TestRemoteProbeScriptEntrecomilla(t *testing.T) {
	dir := t.TempDir()
	testigo := filepath.Join(dir, "ejecutado")
	root := "/var/lib/k'; touch " + testigo + "; echo '"
	runAs := "kindling$(touch " + testigo + ")"
	units := []string{"a.service", "`touch " + testigo + "`"}

	// El guion real, con lo del final cambiado por algo que enseñe las variables.
	buildAs := "kb'; touch " + testigo + "; echo '"
	sc := remoteProbeScript(runAs, buildAs, root, units)
	sc = sc[:len(sc)-len(remoteScript)] + `printf 'root=%s\nrunas=%s\nbuildas=%s\nunits=%s\n' "$ROOT" "$RUNAS" "$BUILDAS" "$UNITS"` + "\n"
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(sc)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	if _, err := os.Stat(testigo); err == nil {
		t.Fatalf("a value was executed by the shell:\n%s", sc)
	}
	want := "root=" + root + "\nrunas=" + runAs + "\nbuildas=" + buildAs + "\nunits=" + strings.Join(units, " ") + "\n"
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

// Sin kernel ni imagen base, `kling up -check` no puede salir en verde: antes
// ni se comprobaban, y todas las líneas eran ✓.
func TestChecksOfSinArtefactos(t *testing.T) {
	p := probe{kvm: true, firecracker: "/usr/local/bin/firecracker", ip: true, iptables: true, nft: true,
		runAs: "kindling", runAsExists: true, buildAs: "kindling-build", buildAsExists: true,
		root: "/var/lib/kindling", remote: true}
	fallan := onlyFailed(checksOf(p))
	var labels []string
	for _, c := range fallan {
		labels = append(labels, c.label)
		if c.fatal {
			t.Errorf("%s is fatal; materialize can still write it", c.label)
		}
	}
	if len(fallan) != 2 || labels[0] != "guest kernel" || labels[1] != "base image" {
		t.Fatalf("failed checks = %v, want the kernel and the base image", labels)
	}
	if checkResult(len(fallan)) == nil {
		t.Fatal("-check with missing artifacts returned no error")
	}
	p.kernel, p.baseImage = true, true
	if f := onlyFailed(checksOf(p)); len(f) != 0 || checkResult(0) != nil {
		t.Fatalf("with everything present: %v", f)
	}
}

// ip, iptables y nft viven en /usr/sbin: un PATH de usuario sin sbin no debe
// darlos por ausentes.
func TestInPathOrSbin(t *testing.T) {
	sbin := t.TempDir()
	if err := os.WriteFile(filepath.Join(sbin, "nft-de-prueba"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sbin, "no-ejecutable"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	viejo := sbinDirs
	sbinDirs = []string{sbin}
	t.Cleanup(func() { sbinDirs = viejo })
	t.Setenv("PATH", t.TempDir())
	if !inPathOrSbin("nft-de-prueba") {
		t.Fatal("a tool in sbin was reported missing")
	}
	if inPathOrSbin("no-ejecutable") || inPathOrSbin("no-existe") {
		t.Fatal("a missing or non-executable tool was reported present")
	}
}

// La orden privilegiada lleva la ruta absoluta de este binario, no `kling` a
// secas, que sudo busca en su secure_path.
func TestPrivilegedSelfRutaAbsoluta(t *testing.T) {
	got := privilegedSelf("daemon", "-root", "/var/lib/k k")
	f := strings.Fields(got)
	i := 0
	if f[0] == "sudo" {
		i = 1
	}
	if !filepath.IsAbs(strings.Trim(f[i], "'")) {
		t.Fatalf("%q: the binary is not an absolute path", got)
	}
	if !strings.HasSuffix(got, "daemon -root '/var/lib/k k'") {
		t.Fatalf("%q: arguments not quoted", got)
	}
}

// Sin el usuario del constructor oci, `up -check` lo dice con la orden para
// crearlo; no es fatal (el constructor corre como root, con aviso).
func TestChecksOfSinUsuarioDeConstruccion(t *testing.T) {
	p := probe{kvm: true, firecracker: "/usr/local/bin/firecracker", ip: true, iptables: true, nft: true,
		runAs: "kindling", runAsExists: true, buildAs: "kindling-build", kernel: true, baseImage: true,
		root: "/var/lib/kindling", remote: true}
	fallan := onlyFailed(checksOf(p))
	if len(fallan) != 1 || fallan[0].label != "user kindling-build" || fallan[0].fatal ||
		len(fallan[0].fix) == 0 || !strings.Contains(fallan[0].fix[0], "useradd") {
		t.Fatalf("failed checks = %+v, want the build user, not fatal, with its useradd", fallan)
	}
}

// En arm64 KVM va compilado dentro: la pista de modprobe kvm_intel no sirve y
// sale la de arm64. Una página de 16 KiB (Pi 5 por defecto) es un aviso, no un
// fallo fatal; en x86, o si el host remoto no supo decirla, no aparece.
func TestChecksOfArm64(t *testing.T) {
	base := probe{kvm: true, firecracker: "/usr/local/bin/firecracker", ip: true, iptables: true, nft: true,
		runAs: "kindling", runAsExists: true, buildAs: "kindling-build", buildAsExists: true,
		kernel: true, baseImage: true, root: "/var/lib/kindling", remote: true}
	find := func(cs []check, label string) (check, bool) {
		for _, c := range cs {
			if c.label == label {
				return c, true
			}
		}
		return check{}, false
	}

	for _, tc := range []struct {
		system   string
		page     int
		pageLine bool // sale la comprobación de la página
		failed   int
		modprobe bool // la pista de /dev/kvm habla de modprobe
	}{
		{"Linux/x86_64", 4096, false, 0, true},
		{"linux/amd64", 0, false, 0, true},
		{"Linux/aarch64", 4096, true, 0, false},
		{"linux/arm64", 16384, true, 1, false},
		{"Linux/aarch64", 0, false, 0, false},
	} {
		p := base
		p.system, p.pageSize = tc.system, tc.page
		cs := checksOf(p)
		pc, ok := find(cs, "host page size")
		if ok != tc.pageLine {
			t.Errorf("%s/%d: page check present=%v, want %v", tc.system, tc.page, ok, tc.pageLine)
		}
		if ok && (pc.fatal || cs[1].label != "host page size") {
			t.Errorf("%s/%d: page check fatal=%v at %q; want a warning right after /dev/kvm",
				tc.system, tc.page, pc.fatal, cs[1].label)
		}
		if f := onlyFailed(cs); len(f) != tc.failed {
			t.Errorf("%s/%d: %d failed checks, want %d", tc.system, tc.page, len(f), tc.failed)
		}
		kv, _ := find(cs, "/dev/kvm")
		if got := strings.Contains(strings.Join(kv.fix, "\n"), "modprobe"); got != tc.modprobe {
			t.Errorf("%s: /dev/kvm fix mentions modprobe=%v, want %v", tc.system, got, tc.modprobe)
		}
	}
}
