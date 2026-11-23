package machine

import (
	"os/user"
	"strings"
	"testing"
)

// Un -run-as que resuelve a uid 0 no baja privilegios: si se diera por
// habilitado, el daemon creería que Firecracker corre sin root y lo lanzaría
// como root igualmente.
func TestRunAsRootNoHabilitaLaBajada(t *testing.T) {
	viejoEuid, viejoGrupo := geteuid, lookupGroup
	t.Cleanup(func() { geteuid, lookupGroup = viejoEuid, viejoGrupo })
	geteuid = func() int { return 0 }
	lookupGroup = func(n string) (*user.Group, error) { return &user.Group{Gid: "36", Name: n}, nil }

	if _, err := user.Lookup("root"); err != nil {
		t.Skipf("este host no tiene usuario root: %v", err)
	}
	priv, aviso := resolvePrivileges("root")
	if priv.Enabled {
		t.Fatalf("resolvePrivileges(root) dio Enabled con uid %d gid %d", priv.UID, priv.GID)
	}
	if !strings.Contains(priv.Motivo, "root") || aviso == "" {
		t.Fatalf("motivo = %q, aviso = %q: debería explicar que el usuario es root", priv.Motivo, aviso)
	}
}
