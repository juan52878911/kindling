package net

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func conDirReservas(t *testing.T) {
	t.Helper()
	antes := DirReservas
	t.Cleanup(func() { DirReservas = antes })
	DirReservas = filepath.Join(t.TempDir(), "net-claims")
}

// Una reserva que tarda más de reservaCaduca en soltarse la puede dar por
// muerta otro daemon y hacerse la suya con el mismo fichero. Al soltar la
// primera, se borraba el fichero sin mirar de quién era: se le soltaba la
// reserva al otro, a mitad de montar su red.
func TestSoltarReservaNoSueltaLaDeOtro(t *testing.T) {
	conDirReservas(t)
	a := Plan(7, "aaaaaaaa")
	if !a.reservarFichero() {
		t.Fatal("la primera reserva no se hizo")
	}
	path := filepath.Join(DirReservas, "7")
	vieja := time.Now().Add(-2 * reservaCaduca)
	if err := os.Chtimes(path, vieja, vieja); err != nil {
		t.Fatal(err)
	}

	b := Plan(7, "bbbbbbbb")
	if !b.reservarFichero() {
		t.Fatal("una reserva caducada no se pudo tomar")
	}
	a.SoltarReserva() // la primera termina, tarde

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("soltar la reserva caducada borró la de quien la tomó después: %v", err)
	}
	c := Plan(7, "cccccccc")
	if c.reservarFichero() {
		t.Fatal("un tercero tomó el índice que el segundo seguía montando")
	}
	b.SoltarReserva()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("la reserva propia no se soltó: %v", err)
	}
}
