//go:build linux

package machine

import (
	"os"
	"path/filepath"
	"testing"
)

// Tras reiniciar el daemon, una máquina que corre en jail se readopta con el
// socket de su chroot, no con el de su directorio: con el segundo, congelarla o
// pararla fallaba con "no such file or directory" hasta destruirla.
func TestSocketDeUnaMaquinaJailed(t *testing.T) {
	m := newTestManager(t)
	id := "0011223344556677"
	sock := m.jailSock(id)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Como firecracker tras el exec de jailer: "--id <id>" en su línea (ver
	// lanzarProcesoFalso).
	cmd, _ := lanzarProcesoFalso(t, "--id", id)
	esperarCmdline(t, cmd.Process.Pid, id)

	if got := m.socketDe(id, cmd.Process.Pid); got != sock {
		t.Fatalf("socketDe = %q, quería el del jail %q", got, sock)
	}
	// Sin jail (o si el proceso ya no es suyo), el de su directorio.
	if got := m.socketDe("8899aabbccddeeff", cmd.Process.Pid); got != m.dir("8899aabbccddeeff")+"/fc.sock" {
		t.Fatalf("sin jail: %q", got)
	}
}
