package scheduler

import (
	"errors"
	"testing"
	"time"
)

// Dos instancias que dan el MISMO id de sesión —dos réplicas restauradas del
// mismo snapshot con el CSPRNG copiado, o un invitado que lo inventa— no pueden
// hacer que la sesión del primer cliente acabe en la microVM del segundo.
func TestBindNoPisaOtraInstancia(t *testing.T) {
	g := New(nil, time.Minute, false, 0)
	a := &entry{machineID: "aaaa", ip: "10.0.0.1"}
	b := &entry{machineID: "bbbb", ip: "10.0.0.2"}

	g.Bind("mismo", "svc", a)
	g.Bind("mismo", "svc", b) // la API vieja tampoco pisa: lo registra y sigue
	if rt := g.Route("mismo"); rt == nil || rt.MachineID() != "aaaa" {
		t.Fatalf("Bind reapuntó la sesión a otra instancia: %+v", rt)
	}
	if err := g.BindGuest("mismo", "g", "svc", b); !errors.Is(err, ErrSessionTaken) {
		t.Fatalf("BindGuest sobre clave ajena: %v", err)
	}
	// Otro servicio con la misma clave tampoco.
	if err := g.BindGuest("mismo", "g", "otro", a); !errors.Is(err, ErrSessionTaken) {
		t.Fatalf("BindGuest de otro servicio: %v", err)
	}
	// Volver a fijarla a la MISMA instancia es legítimo.
	if err := g.BindGuest("mismo", "g2", "svc", a); err != nil {
		t.Fatalf("rebind a la misma instancia: %v", err)
	}
	if got := g.Route("mismo").GuestSID(); got != "g2" {
		t.Fatalf("GuestSID = %q", got)
	}
	// Tras Forget la clave queda libre.
	g.Forget("mismo")
	if err := g.BindGuest("mismo", "g", "svc", b); err != nil {
		t.Fatalf("tras Forget: %v", err)
	}
}

// Con claves acuñadas, el mismo id del invitado en dos instancias da dos rutas
// independientes, cada una con su instancia y el id del invitado aparte.
func TestBindGuestClavesAcunadas(t *testing.T) {
	g := New(nil, time.Minute, false, 0)
	a := &entry{machineID: "aaaa"}
	b := &entry{machineID: "bbbb"}
	k1, k2 := NewSessionKey(), NewSessionKey()
	if k1 == k2 || len(k1) != 32 {
		t.Fatalf("claves: %q %q", k1, k2)
	}
	if err := g.BindGuest(k1, "repetido", "svc", a); err != nil {
		t.Fatal(err)
	}
	if err := g.BindGuest(k2, "repetido", "svc", b); err != nil {
		t.Fatal(err)
	}
	r1, r2 := g.Route(k1), g.Route(k2)
	if r1.MachineID() != "aaaa" || r2.MachineID() != "bbbb" ||
		r1.GuestSID() != "repetido" || r2.GuestSID() != "repetido" {
		t.Fatalf("rutas mezcladas: %+v %+v", r1, r2)
	}
	if g.Route("repetido") != nil {
		t.Fatal("el id del invitado no debe servir como clave")
	}
}
