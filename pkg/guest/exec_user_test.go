package guest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// api.ExecUserService es el usuario del servicio de la imagen (el user: del
// compose): con él corre el healthcheck, no como root.
func TestCredencialExecUsuarioDelServicio(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root:x:0:0::/root:/bin/sh\npostgres:x:70:70::/var/lib/postgresql:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc/group"), []byte("postgres:x:70:\n"), 0o644)

	serviceState.mu.Lock()
	antes := serviceState.svc
	serviceState.svc = &Service{spec: api.ServiceSpec{User: "postgres"}, root: root}
	serviceState.mu.Unlock()
	defer func() {
		serviceState.mu.Lock()
		serviceState.svc = antes
		serviceState.mu.Unlock()
	}()

	cred, home, err := credencialExec(api.ExecUserService)
	if err != nil || cred == nil || cred.Uid != 70 || cred.Gid != 70 || home != "/var/lib/postgresql" {
		t.Fatalf("cred = %+v, home %q, %v", cred, home, err)
	}
	// Sin usuario en el servicio, root (nil).
	serviceState.mu.Lock()
	serviceState.svc = &Service{spec: api.ServiceSpec{}, root: root}
	serviceState.mu.Unlock()
	if cred, _, err := credencialExec(api.ExecUserService); err != nil || cred != nil {
		t.Fatalf("sin usuario: %+v, %v", cred, err)
	}
	// Un uid suelto vale aunque no esté en /etc/passwd.
	if cred, _, err := credencialExec("1234"); err != nil || cred == nil || cred.Uid != 1234 {
		t.Fatalf("uid: %+v, %v", cred, err)
	}
}
