//go:build darwin

package machine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func plataformaConProc() bool { return true }

// proc_pid_rusage da los despertares y la E/S de disco del proceso: escribir
// y sincronizar con F_FULLFSYNC 4 MiB los hace subir.
func TestCPUProcesoDespertaresYDisco(t *testing.T) {
	antes, ok := cpuProceso(os.Getpid())
	if !ok || !antes.conDespertares || !antes.conDisco {
		t.Fatalf("rusage sin despertares o disco: %+v", antes)
	}
	if antes.despReposo+antes.despInterrupcion == 0 {
		t.Errorf("ningún despertar: %+v", antes)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "d"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	for range 4 {
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_FULLFSYNC, 0); e != 0 {
		t.Fatal(e)
	}
	despues, _ := cpuProceso(os.Getpid())
	if d := despues.escritos - antes.escritos; d < 4<<20 {
		t.Errorf("escritos subió %d bytes, quería ≥ 4 MiB", d)
	}
}

// La huella y la residente del propio proceso, en MiB.
func TestMemoriaPropia(t *testing.T) {
	h, rss := memoriaPropia(os.Getpid())
	if h <= 0 || rss <= 0 {
		t.Fatalf("huella %d, rss %d", h, rss)
	}
	if h0, _ := memoriaPropia(1 << 30); h0 != 0 {
		t.Fatalf("un PID que no existe tiene huella %d", h0)
	}
}
