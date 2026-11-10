package guest

import (
	"testing"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// El error de un trozo diferido es del handle que lo escribió: con dos
// descriptores del mismo fichero, cerrar el otro antes no se lo lleva (antes
// sí, y el que había escrito cerraba limpio y perdía sus datos sin saberlo).
func TestWriteBehindErrorEsDelHandle(t *testing.T) {
	m, _ := montarWB(t)
	_, node, _ := m.lookup(t, rootID, "hello.txt")
	// fh1 solo para leer: el daemon rechaza su write (EBADF), como un ENOSPC.
	e, fh1 := m.open(t, node, linuxORdonly)
	if e != 0 {
		t.Fatalf("open fh1: %d", e)
	}
	e, fh2 := m.open(t, node, linuxORdwr)
	if e != 0 {
		t.Fatalf("open fh2: %d", e)
	}
	if e, _ := m.dev.pedir(t, opWrite, node, writeIn(fh1, 0, 3), []byte("xyz")); e != 0 {
		t.Fatalf("write fh1 (deferred): %d", e)
	}
	// Lo de fh1 ya llegó (y falló) antes de contestar a esto: fh2 no lo ve.
	if e, _ := m.dev.pedir(t, opFsync, node, u64s(fh2, 0, 0)); e != 0 {
		t.Fatalf("fsync fh2: %d, want 0 (the error is fh1's)", e)
	}
	if e, _ := m.dev.pedir(t, opWrite, node, writeIn(fh2, 0, 2), []byte("ok")); e != 0 {
		t.Fatalf("write fh2: %d, want 0 (the error is fh1's)", e)
	}
	if e, _ := m.dev.pedir(t, opFlush, node, u64s(fh2, 0, 0)); e != 0 {
		t.Fatalf("flush fh2: %d, want 0 (the error is fh1's)", e)
	}
	if e, _ := m.dev.pedir(t, opFlush, node, u64s(fh1, 0, 0)); e != share.EBADF {
		t.Fatalf("flush fh1: %d, want EBADF", e)
	}
	if e, _ := m.dev.pedir(t, opFlush, node, u64s(fh1, 0, 0)); e != 0 {
		t.Fatalf("second flush fh1: %d, want 0 (reported once)", e)
	}
	m.dev.pedir(t, opRelease, node, u64s(fh2, 0, 0))
	m.dev.pedir(t, opRelease, node, u64s(fh1, 0, 0))
}

// Un error sin dueño (el estado restaurado de kling-hv guarda uno por nodo) lo
// recibe el primero que pregunte, como antes, y una sola vez.
func TestWriteBehindErrorSinDueño(t *testing.T) {
	st := &wbState{}
	a, b := &fileHandle{}, &fileHandle{}
	st.err = share.ENOSPC // lo que pone fuse_host.go al restaurar
	st.apuntarErr(a, share.EIO)
	if e := st.tomarErr(b, true); e != share.ENOSPC {
		t.Fatalf("b took %d, want the ownerless ENOSPC", e)
	}
	if e := st.tomarErr(b, true); e != 0 {
		t.Fatalf("b took %d again, want 0 (a's EIO is not b's)", e)
	}
	if st.err != share.EIO {
		t.Fatalf("err = %d, want a's EIO kept for the saved state", st.err)
	}
	if e := st.tomarErr(a, true); e != share.EIO {
		t.Fatalf("a took %d, want EIO", e)
	}
	if st.err != 0 {
		t.Fatalf("err = %d after taking everything", st.err)
	}
}
