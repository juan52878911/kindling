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

// El rol del llamante del sistema se resuelve una vez por conexión; una
// conexión nueva lo vuelve a resolver, y lo que no es fiable (consulta de
// grupos vacía, sin rol) no se recuerda.
func TestRolSeRecuerdaPorConexion(t *testing.T) {
	p := politicaDePrueba(t)
	consultas := 0
	devolver := []int{gidEquipoC}
	p.grupos = func(int) []int { consultas++; return devolver }
	s := &Server{authz: p}
	var ll Llamante
	s.identificar = func(net.Conn) (Llamante, error) { return ll, nil }
	pedir := func(ctx context.Context) (Rol, error) {
		t.Helper()
		r := httptest.NewRequest("GET", "/machines", nil).WithContext(ctx)
		return s.rolDePeticion(r)
	}

	ll = Llamante{UID: uidDeC, GID: 1, Conocido: true}
	con := s.conContexto(context.Background(), nil)
	for i := 0; i < 5; i++ {
		if rol, err := pedir(con); err != nil || rol.Tenant != "c" {
			t.Fatalf("petición %d: %+v %v", i, rol, err)
		}
	}
	if consultas != 1 {
		t.Fatalf("5 peticiones de una conexión consultaron los grupos %d veces, quiero 1", consultas)
	}
	if _, err := pedir(s.conContexto(context.Background(), nil)); err != nil || consultas != 2 {
		t.Fatalf("conexión nueva: consultas = %d, %v; quiero 2", consultas, err)
	}

	// La consulta vuelve vacía (lo mismo que un fallo de la base de usuarios):
	// sin rol en esa petición, y la siguiente vuelve a consultar.
	devolver = nil
	con = s.conContexto(context.Background(), nil)
	if _, err := pedir(con); err == nil {
		t.Fatal("sin grupos, el uid de c no debía tener rol")
	}
	devolver = []int{gidEquipoC}
	if rol, err := pedir(con); err != nil || rol.Tenant != "c" {
		t.Fatalf("tras el fallo pasajero: %+v %v", rol, err)
	}
	if consultas != 4 {
		t.Fatalf("consultas = %d, quiero 4 (el vacío no se recuerda)", consultas)
	}

	// Un uid sin rol (con grupos, pero ninguno con regla) tampoco se
	// recuerda; el token se mira por petición.
	devolver = []int{2}
	ll = Llamante{UID: uidNadie, GID: 1, Conocido: true}
	con = s.conContexto(context.Background(), nil)
	antes := consultas
	for i := 0; i < 2; i++ {
		if _, err := pedir(con); err == nil {
			t.Fatal("uid sin regla con rol")
		}
	}
	if consultas != antes+2 {
		t.Fatalf("consultas = %d, quiero %d", consultas, antes+2)
	}
	r := httptest.NewRequest("GET", "/machines", nil).WithContext(con)
	r.Header.Set("Authorization", "Bearer "+tokenDeA)
	if rol, err := s.rolDePeticion(r); err != nil || rol.Tenant != "a" {
		t.Fatalf("token en una conexión sin rol: %+v %v", rol, err)
	}
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
