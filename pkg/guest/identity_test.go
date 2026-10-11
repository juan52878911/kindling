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
