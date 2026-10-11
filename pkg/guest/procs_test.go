package guest

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// proc escribe un /proc/<pid> de mentira.
func proc(t *testing.T, root string, pid int, stat, cmdline string) {
	t.Helper()
	d := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644)
	os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmdline), 0o644)
}

func TestListProcs(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 1, "1 (sh) S 0 1 1 0", "/bin/sh\x00/entrypoint\x00")      // quien lanzó al agente
	proc(t, root, 2, "2 (kthreadd) S 0 0 0 0", "")                          // kernel
	proc(t, root, 9, "9 (kworker/0:1) I 2 0 0 0", "")                       // kernel
	proc(t, root, 50, "50 (kling-guest) S 1 1 1 0", "/usr/local/bin/x\x00") // el agente
	proc(t, root, 60, "60 (node) S 50 60 60 0", "node\x00srv.js\x00")       // el servicio
	proc(t, root, 61, "61 (node (worker)) S 60 60 60 0", "node\x00")        // un hijo suyo, con paréntesis
	proc(t, root, 70, "70 (python3) S 1 70 70 0", "python3\x00")            // otro, fuera del servicio
	proc(t, root, 80, "80 (sh) Z 60 60 60 0", "")                           // zombi
	os.WriteFile(filepath.Join(root, "self"), nil, 0o644)                   // no es un pid

	got := listProcs(root, 50, 0)
	if got.Count != 3 || got.Covered != 0 || !slices.Equal(got.Names, []string{"node", "node (worker)", "python3"}) {
		t.Fatalf("sin servicio cubierto = %+v", got)
	}
	got = listProcs(root, 50, 60)
	if got.Count != 3 || got.Covered != 2 {
		t.Fatalf("con el servicio en el grupo 60 = %+v, quiero 2 cubiertos de 3", got)
	}
}

func TestListProcsSoloElAgente(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 1, "1 (kling-guest) S 0 1 1 0", "/usr/local/bin/kling-guest\x00")
	if got := listProcs(root, 1, 0); got.Count != 0 || len(got.Names) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestListeningPorts(t *testing.T) {
	root := t.TempDir()
	proc(t, root, 60, "60 (node) S 1 60 60 0", "node\x00")
	proc(t, root, 61, "61 (node) S 60 60 60 0", "node\x00")
	proc(t, root, 70, "70 (other) S 1 70 70 0", "other\x00")
	fd := func(pid int, n, target string) {
		d := filepath.Join(root, strconv.Itoa(pid), "fd")
		os.MkdirAll(d, 0o755)
		if err := os.Symlink(target, filepath.Join(d, n)); err != nil {
			t.Fatal(err)
		}
	}
	fd(60, "3", "socket:[111]") // escucha en 9000
	fd(61, "4", "socket:[222]") // escucha en 8443 (tcp6)
	fd(60, "5", "socket:[333]") // una conexión establecida, no cuenta
	fd(70, "3", "socket:[444]") // escucha, pero es de otro grupo
	fd(60, "6", "/dev/null")
	os.MkdirAll(filepath.Join(root, "net"), 0o755)
	os.WriteFile(filepath.Join(root, "net/tcp"), []byte(
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
			"   0: 00000000:2328 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1 0 100 0 0 10 0\n"+
			"   1: 0100007F:2328 0100007F:A1B2 01 00000000:00000000 00:00000000 00000000     0        0 333 1 0 100 0 0 10 0\n"+
			"   2: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 444 1 0 100 0 0 10 0\n"), 0o644)
	os.WriteFile(filepath.Join(root, "net/tcp6"), []byte(
		"  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
			"   0: 00000000000000000000000000000000:20FB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 222 1 0 100 0 0 10 0\n"), 0o644)
	got := listeningPorts(root, 60)
	if len(got) != 2 || !got[9000] || !got[8443] {
		t.Fatalf("puertos = %v, quiero 9000 y 8443", got)
	}
	if got := listeningPorts(root, 99); len(got) != 0 {
		t.Fatalf("grupo sin procesos = %v", got)
	}
}
