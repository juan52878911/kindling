package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os/user"
	"strconv"
	"testing"
)

// servidorConReglaDeGrupo da un servidor cuya política tiene una regla de grupo
// por delante de la del uid de "nobody", con la base de usuarios de verdad
// (gruposSistema): el caso de J-06, donde el rol de cada petición pasa por
// user.LookupId y GroupIds. llamadas cuenta las resoluciones de grupos.
func servidorConReglaDeGrupo(tb testing.TB) (*Server, *int) {
	tb.Helper()
	u, err := user.Lookup("nobody")
	if err != nil {
		tb.Skipf("sin usuario nobody: %v", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	js := fmt.Sprintf(`{"rules": [
	    {"group": "equipo-x", "role": "tenant:x"},
	    {"uid": %d, "role": "tenant:n"}
	]}`, uid)
	res := resolutor{
		usuario: func(string) (int, error) { return 0, errors.New("unknown user") },
		grupo:   func(string) (int, error) { return 424242, nil },
	}
	p, err := parsePolitica([]byte(js), res)
	if err != nil {
		tb.Fatal(err)
	}
	if p.admins[uid] {
		tb.Skip("nobody es admin implícito aquí")
	}
	llamadas := 0
	p.grupos = func(uid int) []int { llamadas++; return gruposSistema(uid) }
	s := &Server{authz: p}
	s.identificar = func(net.Conn) (Llamante, error) {
		return Llamante{UID: uid, GID: gid, Conocido: true}, nil
	}
	return s, &llamadas
}

// BenchmarkRolDePeticion mide el coste de autorizar cada petición de una
// misma conexión (keep-alive) con una regla de grupo por delante.
func BenchmarkRolDePeticion(b *testing.B) {
	s, llamadas := servidorConReglaDeGrupo(b)
	ctx := s.conContexto(context.Background(), nil)
	r := httptest.NewRequest("POST", "/machines/m/exec", nil).WithContext(ctx)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rol, err := s.rolDePeticion(r)
		if err != nil || rol.Tenant != "n" {
			b.Fatalf("%+v %v", rol, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(*llamadas)/float64(b.N), "lookups/op")
}
