package daemon

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
)

func TestParseCuotas(t *testing.T) {
	res := resolutor{
		usuario: func(string) (int, error) { return 0, errors.New("unknown user") },
		grupo:   func(string) (int, error) { return 0, errors.New("unknown group") },
	}
	malas := map[string]string{
		"campo desconocido": `{"quotas":{"a":{"max_vms":1}}}`,
		"negativa":          `{"quotas":{"a":{"max_machines":-1}}}`,
		"negativa en *":     `{"quotas":{"*":{"max_mem_mib":-5}}}`,
		"nombre inválido":   `{"quotas":{"Mal Nombre":{"max_machines":1}}}`,
		"no es un objeto":   `{"quotas":{"a":3}}`,
	}
	for n, js := range malas {
		if _, err := parsePolitica([]byte(js), res); err == nil {
			t.Errorf("%s: debía fallar", n)
		}
	}

	p, err := parsePolitica([]byte(`{"quotas":{
	  "*": {"max_machines": 5, "max_mem_mib": 4096},
	  "a": {"max_machines": 20, "max_disk_mib": 0},
	  "b": {}
	}}`), res)
	if err != nil {
		t.Fatal(err)
	}
	casos := map[string]machine.Cuota{
		// Lo suyo manda; lo que no dice, de "*"; lo que nadie dice, sin tope.
		"a": {Maquinas: 20, MemMiB: 4096, DiscoMiB: 0},
		"b": {Maquinas: 5, MemMiB: 4096, DiscoMiB: machine.SinTope},
		// Sin entrada propia: la de "*".
		"c": {Maquinas: 5, MemMiB: 4096, DiscoMiB: machine.SinTope},
	}
	for t2, want := range casos {
		if got, ok := p.cuotaDe(t2); !ok || got != want {
			t.Errorf("%s: %+v %v, quería %+v", t2, got, ok, want)
		}
	}
	// Sin "*", quien no tiene la suya no tiene cuota.
	p, err = parsePolitica([]byte(`{"quotas":{"a":{"max_machines":1}}}`), res)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.cuotaDe("b"); ok {
		t.Error("b sin cuota propia ni \"*\" tiene cuota")
	}
	if c, _ := p.cuotaDe("a"); c != (machine.Cuota{Maquinas: 1, MemMiB: machine.SinTope, DiscoMiB: machine.SinTope}) {
		t.Errorf("a: %+v", c)
	}
	if p, _ := parsePolitica([]byte(`{"rules":[]}`), res); p.tieneCuotas() {
		t.Error("una política sin quotas tiene cuotas")
	}
}

// De punta a punta por las rutas de verdad: SetAuthz le pasa las cuotas al
// manager, y un run que no cabe es un 429 que dice qué tope y cuánto lleva.
func TestCuotasPorElAPI(t *testing.T) {
	pol := politicaDePrueba(t)
	if err := pol.resolverCuotas(map[string]cuotaFichero{
		"*": {MaxMachines: ptr(1)},
		"a": {MaxMachines: ptr(2)},
	}); err != nil {
		t.Fatal(err)
	}
	s := servidorAuthz(t, nil)
	// En Linux, sin -run-as, el manager se niega a arrancar nada antes de
	// llegar a la cuota (jailer): aquí no se arranca nada de verdad.
	s.mgr.JailerBlocked = ""
	s.SetAuthz(pol)
	h := s.routes()

	// a tiene dos máquinas (congeladas: cuentan) y su tope es 2; b tiene una
	// y el de "*" es 1.
	for _, c := range []struct {
		uid  int
		want string
	}{
		{uidA, `tenant \"a\" quota exceeded: max_machines is 2 and it has 2 machine(s)`},
		{uidB, `tenant \"b\" quota exceeded: max_machines is 1 and it has 1 machine(s)`},
	} {
		rr := como(t, h, c.uid, "POST", "/machines", `{"image":"min"}`)
		if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("uid %d: %d %s", c.uid, rr.Code, rr.Body)
		}
	}
	// c no tiene nada: pasa la cuota (y falla después, sin imagen de verdad,
	// por otra cosa).
	if rr := como(t, h, uidDeC, "POST", "/machines", `{"image":"min"}`); rr.Code == http.StatusTooManyRequests {
		t.Errorf("c sin máquinas: %d %s", rr.Code, rr.Body)
	}
	// Un admin sin etiqueta de dueño no tiene cuota.
	if rr := como(t, h, uidAdmin, "POST", "/machines", `{"image":"min"}`); rr.Code == http.StatusTooManyRequests {
		t.Errorf("admin: %d %s", rr.Code, rr.Body)
	}
	// Sin cuotas en la política, el mismo run de a no topa.
	s.SetAuthz(politicaDePrueba(t))
	if rr := como(t, h, uidA, "POST", "/machines", `{"image":"min"}`); rr.Code == http.StatusTooManyRequests {
		t.Errorf("sin cuotas: %d %s", rr.Code, rr.Body)
	}
}

func ptr(n int) *int { return &n }
