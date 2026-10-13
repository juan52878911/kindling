package guest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestRenewMachineID(t *testing.T) {
	dir := t.TempDir()
	viejo := "0123456789abcdef0123456789abcdef\n"
	etc := filepath.Join(dir, "etc-machine-id")
	dbus := filepath.Join(dir, "dbus-machine-id")
	vacio := filepath.Join(dir, "vacio")
	enlace := filepath.Join(dir, "enlace")
	os.WriteFile(etc, []byte(viejo), 0o444)
	os.WriteFile(dbus, []byte(viejo), 0o644)
	os.WriteFile(vacio, nil, 0o444) // "genéralo al arrancar": no se toca
	os.Symlink(etc, enlace)

	got := renewMachineID([]string{etc, dbus, vacio, enlace, filepath.Join(dir, "no-existe")})
	if len(got) != 2 || got[0] != etc || got[1] != dbus {
		t.Fatalf("renovados = %v, quiero %s y %s", got, etc, dbus)
	}
	a, _ := os.ReadFile(etc)
	b, _ := os.ReadFile(dbus)
	if string(a) == viejo || string(a) != string(b) || !esMachineID(strings.TrimSpace(string(a))) {
		t.Fatalf("machine-id nuevo %q / %q", a, b)
	}
	if fi, _ := os.Stat(etc); fi.Mode().Perm() != 0o444 {
		t.Errorf("permisos %v, quiero los de antes (0444)", fi.Mode().Perm())
	}
	if c, _ := os.ReadFile(vacio); len(c) != 0 {
		t.Error("tocó un machine-id vacío")
	}
	if fi, _ := os.Lstat(enlace); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("sustituyó el enlace simbólico")
	}
}

// /resync renueva el machine-id solo en una copia (instance), no al descongelar.
func TestResyncRenuevaMachineIDSoloEnCopias(t *testing.T) {
	falsos(t, nil, nil)
	f := filepath.Join(t.TempDir(), "machine-id")
	viejo := []string{machineIDFiles[0], machineIDFiles[1]}
	machineIDFiles = []string{f}
	t.Cleanup(func() { machineIDFiles = viejo })
	os.WriteFile(f, []byte("0123456789abcdef0123456789abcdef\n"), 0o444)
	ent := bytes.Repeat([]byte{7}, api.GuestResyncEntropy)
	for _, c := range []struct {
		restore string
		cambia  bool
	}{{api.ResyncThaw, false}, {api.ResyncInstance, true}} {
		antes, _ := os.ReadFile(f)
		body, _ := json.Marshal(api.GuestResync{UnixNano: time.Now().UnixNano(), Entropy: ent, Restore: c.restore})
		rr := postResync(t, string(body))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.restore, rr.Code, rr.Body)
		}
		var res api.GuestResyncResult
		json.Unmarshal(rr.Body.Bytes(), &res)
		despues, _ := os.ReadFile(f)
		if cambio := string(antes) != string(despues); cambio != c.cambia || (len(res.Renewed) == 1) != c.cambia {
			t.Fatalf("%s: cambió %v, renewed %v; quiero cambio %v", c.restore, cambio, res.Renewed, c.cambia)
		}
	}
}

func TestHostKeys(t *testing.T) {
	dir := t.TempDir()
	viejo := sshHostKeyGlob
	sshHostKeyGlob = filepath.Join(dir, "ssh_host_*_key")
	t.Cleanup(func() { sshHostKeyGlob = viejo })
	os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key.pub"), nil, 0o644)
	if k := hostKeys(); len(k) != 1 || filepath.Base(k[0]) != "ssh_host_ed25519_key" {
		t.Fatalf("%v", k)
	}
}

// sshFalso deja claves de host en un temporal y un ssh-keygen de mentira que
// escribe "nueva <tipo>" en el fichero que le pidan y apunta sus argumentos.
func sshFalso(t *testing.T, tipos ...string) (dir, registro string) {
	t.Helper()
	dir = t.TempDir()
	registro = filepath.Join(dir, "args")
	gen := filepath.Join(dir, "ssh-keygen")
	os.WriteFile(gen, []byte(`#!/bin/sh
echo "$@" >> `+registro+`
while [ $# -gt 0 ]; do case "$1" in -t) t=$2; shift;; -f) f=$2; shift;; esac; shift; done
echo "nueva $t" > "$f"; echo pub > "$f.pub"
`), 0o755)
	for _, ty := range tipos {
		os.WriteFile(filepath.Join(dir, "ssh_host_"+ty+"_key"), []byte("vieja"), 0o600)
		os.WriteFile(filepath.Join(dir, "ssh_host_"+ty+"_key.pub"), []byte("vieja"), 0o644)
	}
	viejoGlob, viejoGen := sshHostKeyGlob, sshKeygenPaths
	sshHostKeyGlob, sshKeygenPaths = filepath.Join(dir, "ssh_host_*_key"), []string{gen}
	t.Cleanup(func() { sshHostKeyGlob, sshKeygenPaths = viejoGlob, viejoGen })
	return dir, registro
}

func TestSSHRenewalRegeneraCadaTipo(t *testing.T) {
	dir, registro := sshFalso(t, "ed25519", "rsa")
	viejo := procRoot
	procRoot = t.TempDir() // sin sshd: nadie a quien mandar el HUP
	t.Cleanup(func() { procRoot = viejo })
	r := sshRenewal()
	if r == nil {
		t.Fatal("sshRenewal = nil con claves y ssh-keygen")
	}
	if err := r(); err != nil {
		t.Fatal(err)
	}
	for _, ty := range []string{"ed25519", "rsa"} {
		if b, _ := os.ReadFile(filepath.Join(dir, "ssh_host_"+ty+"_key")); string(b) != "nueva "+ty+"\n" {
			t.Errorf("%s: %q", ty, b)
		}
	}
	args, _ := os.ReadFile(registro)
	if strings.Count(string(args), "-t ") != 2 || strings.Contains(string(args), "ecdsa") || !strings.Contains(string(args), "-N  -C ") {
		t.Errorf("ssh-keygen recibió %q: un tipo por clave existente, sin frase", args)
	}
}

func TestSSHRenewalSinKeygenOSinClaves(t *testing.T) {
	sshFalso(t, "ed25519")
	sshKeygenPaths = []string{"/no/existe"}
	if sshRenewal() != nil {
		t.Fatal("sin ssh-keygen no hay renovación (la avisa save)")
	}
	sshFalso(t)
	if sshRenewal() != nil {
		t.Fatal("sin claves no hay renovación")
	}
}

func TestSSHDListeners(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 1, "1 (kling-guest) S 0 1 1 0", "x\x00")
	proc(t, root, 40, "40 (sshd) S 1 40 40 0", "sshd: /usr/sbin/sshd [listener]\x00")
	proc(t, root, 41, "41 (sshd-session) S 40 41 41 0", "sshd-session\x00")
	proc(t, root, 42, "42 (sshd) S 41 41 41 0", "sshd: root@pts/0\x00")  // sesión: hija de sshd-session
	proc(t, root, 50, "50 (sshd) S 40 50 50 0", "sshd: root [priv]\x00") // sesión vieja: hija de sshd
	got := sshdListeners(root)
	if len(got) != 1 || got[0] != 40 {
		t.Fatalf("sshdListeners = %v, quiero [40]", got)
	}
}
