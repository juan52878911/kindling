package osb

import "testing"

// Al borrar un sandbox, el shim suelta la salida guardada de sus comandos:
// antes la retenía hasta que otros 256 comandos la empujaban fuera.
func TestBorrarOlvidaLosComandos(t *testing.T) {
	b := nuevoBanco(t)
	if st, _, _ := b.execd(t, "POST", "/command", `{"command":"echo hola"}`); st != 200 {
		t.Fatalf("command: %d", st)
	}
	// Las entradas del sandbox en cmds más las de cmdOrden (en el banco no
	// hay otros sandboxes).
	cuantos := func() int {
		b.s.mu.Lock()
		defer b.s.mu.Unlock()
		n := 0
		for _, c := range b.s.cmds {
			if c.sandbox == idGW {
				n++
			}
		}
		return n + len(b.s.cmdOrden)
	}
	if cuantos() == 0 {
		t.Fatal("el comando no quedó registrado")
	}
	if st, _, _ := b.pedir(t, "DELETE", "/v1/sandboxes/"+idOSB, ""); st != 204 {
		t.Fatalf("delete: %d", st)
	}
	if n := cuantos(); n != 0 {
		t.Errorf("quedan %d entradas de comandos del sandbox borrado", n)
	}
}
