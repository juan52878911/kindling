package frontal

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func ptr[T any](v T) *T { return &v }

func TestLoteSeReparteEntreHosts(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b})
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(6)})
	esperaCodigo(t, resp, 201)
	out := decodifica[LoteRespuesta](t, resp)
	if len(out.Sandboxes) != 6 || len(out.Errors) != 0 {
		t.Fatalf("got %d sandboxes, %d errors; want 6, 0", len(out.Sandboxes), len(out.Errors))
	}
	if a.creadas != 3 || b.creadas != 3 {
		t.Fatalf("spread a=%d b=%d, want 3 and 3", a.creadas, b.creadas)
	}
	ids := map[string]bool{}
	for _, sb := range out.Sandboxes {
		ids[sb.ID] = true
	}
	if len(ids) != 6 {
		t.Fatalf("ids not unique: %v", ids)
	}
	// El lote de uno también responde con la forma de lote.
	resp = pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(1)})
	esperaCodigo(t, resp, 201)
	if o := decodifica[LoteRespuesta](t, resp); len(o.Sandboxes) != 1 {
		t.Fatalf("count 1: %+v", o)
	}
}

func TestLoteTopes(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 3, MaxLote: 4})
	for _, n := range []int{0, -1, 5} {
		esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(n)}), 400)
	}
	// Cuota: 4 no caben con tope 3, y no se crea NINGUNO.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(4)}), 429)
	if f.intentos != 0 {
		t.Fatalf("%d creations attempted over quota", f.intentos)
	}
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(3)}), 429)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(2)}), 201)
	if f.creadas != 3 {
		t.Fatalf("%d created, want 3", f.creadas)
	}
}

func TestLoteTopeDeMemoriaCuentaElLoteEntero(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxMemTotalMiB: 1024})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", MemMiB: 512, Count: ptr(3)}), 429)
	if f.intentos != 0 {
		t.Fatal("created over the memory quota")
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", MemMiB: 512, Count: ptr(2)}), 201)
}

func TestLoteAtomicoLimpiaAlFallarUnoAMedias(t *testing.T) {
	f := nuevoFalso(t)
	f.crearFallaDesde = 3
	srv, s := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 5})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(5), Atomic: true}), 500)
	if n := f.grafosVivos(); n != 0 {
		t.Fatal(n)
	}
	f.mu.Lock()
	vivas := len(f.maquinas)
	f.mu.Unlock()
	if vivas != 0 {
		t.Fatalf("%d machines left after an atomic failure", vivas)
	}
	if f.creadas == 0 {
		t.Fatal("the fake never created anything: the test proves nothing")
	}
	// La reserva de cuota se devolvió.
	s.mu.Lock()
	resto := len(s.enVuelo)
	s.mu.Unlock()
	if resto != 0 {
		t.Fatalf("in-flight quota not released: %v", s.enVuelo)
	}
	f.mu.Lock()
	f.crearFallaDesde = 0
	f.mu.Unlock()
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(5), Atomic: true}), 201)
}

func TestLoteNoAtomicoDevuelveLosQueSalieron(t *testing.T) {
	f := nuevoFalso(t)
	f.crearFallaDesde = 3
	srv, _ := montar(t, map[string]*falso{"a": f})
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(5)})
	esperaCodigo(t, resp, 207)
	out := decodifica[LoteRespuesta](t, resp)
	if len(out.Sandboxes) != 2 || len(out.Errors) != 3 {
		t.Fatalf("got %d ok, %d errors; want 2 and 3", len(out.Sandboxes), len(out.Errors))
	}
	if out.Errors[0].Status != 500 || out.Errors[0].Error == "" {
		t.Fatalf("error entry: %+v", out.Errors[0])
	}
	// Si fallan todos es un error como el de uno suelto.
	f.mu.Lock()
	f.crearFallaDesde = 1
	f.mu.Unlock()
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(2)}), 500)
}

func TestAtomicSinCountEsError(t *testing.T) {
	srv, _ := montar(t, map[string]*falso{"a": nuevoFalso(t)})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Atomic: true}), 400)
}

// ---- volúmenes

var alice = &Tenant{Nombre: "alice"}
var bob = &Tenant{Nombre: "bob"}

func volDe(t *Tenant, n string) *api.Volume {
	return &api.Volume{Name: nombreReal(t, n), SizeBytes: 64 << 20, Labels: etiquetasVolumen(t)}
}

func TestVolumenPropioSeMonta(t *testing.T) {
	f := nuevoFalso(t)
	f.sembrarVolumen(volDe(alice, "datos"))
	srv, _ := montar(t, map[string]*falso{"a": f})
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Volumes: []VolumenPeticion{{Name: "datos", Mount: "/data", ReadOnly: true}}})
	f.mu.Lock()
	defer f.mu.Unlock()
	vs := f.peticiones[0].Volumes
	if len(vs) != 1 || vs[0].Name != nombreReal(alice, "datos") || vs[0].Mount != "/data" || !vs[0].ReadOnly {
		t.Fatalf("daemon got volumes %+v", vs)
	}
}

func TestVolumenAjenoEsNoEncontrado(t *testing.T) {
	f := nuevoFalso(t)
	f.sembrarVolumen(volDe(bob, "secreto"))
	// Un volumen con el nombre real de bob pero sin su prefijo/etiqueta de
	// alice tampoco se monta.
	f.sembrarVolumen(&api.Volume{Name: nombreReal(alice, "robado"), Labels: etiquetasVolumen(bob)})
	srv, _ := montar(t, map[string]*falso{"a": f})
	for _, n := range []string{"secreto", "robado"} {
		esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes",
			CrearPeticion{Image: "base", Volumes: []VolumenPeticion{{Name: n, Mount: "/d"}}}), 404)
		esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/volumes/"+n, nil), 404)
		esperaCodigo(t, pide(t, srv, tokenAlice, "DELETE", "/v1/volumes/"+n, nil), 404)
	}
	if f.intentos != 0 || !f.tieneVolumen(nombreReal(bob, "secreto")) {
		t.Fatal("foreign volume touched")
	}
	// Ni clonar de él.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "copia", CloneFrom: "secreto"}), 404)
	// Y alice no lo ve en la lista.
	l := decodifica[[]Volumen](t, pide(t, srv, tokenAlice, "GET", "/v1/volumes", nil))
	if len(l) != 0 {
		t.Fatalf("alice sees %+v", l)
	}
}

func TestVolumenEligeElHostQueLoTiene(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	b.sembrarVolumen(volDe(alice, "datos"))
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b})
	// Con un lote de ro, todos van al host del volumen aunque el reparto
	// normal pusiera la mitad en a.
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Count: ptr(4),
		Volumes: []VolumenPeticion{{Name: "datos", Mount: "/data", ReadOnly: true}}})
	esperaCodigo(t, resp, 201)
	if a.creadas != 0 || b.creadas != 4 {
		t.Fatalf("a=%d b=%d, want 0 and 4", a.creadas, b.creadas)
	}
}

func TestVolumenesEnHostsDistintosEsConflicto(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	a.sembrarVolumen(volDe(alice, "uno"))
	b.sembrarVolumen(volDe(alice, "dos"))
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b})
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base",
		Volumes: []VolumenPeticion{{Name: "uno", Mount: "/uno"}, {Name: "dos", Mount: "/dos"}}})
	esperaCodigo(t, resp, 409)
	e := decodifica[api.Error](t, resp)
	if e.Message == "" || a.intentos+b.intentos != 0 {
		t.Fatalf("message %q, attempts %d", e.Message, a.intentos+b.intentos)
	}
}

func TestVolumenValidacion(t *testing.T) {
	f := nuevoFalso(t)
	f.sembrarVolumen(volDe(alice, "datos"))
	srv, _ := montar(t, map[string]*falso{"a": f})
	malos := map[string]CrearPeticion{
		"rw en lote":     {Image: "base", Count: ptr(2), Volumes: []VolumenPeticion{{Name: "datos", Mount: "/d"}}},
		"mount relativo": {Image: "base", Volumes: []VolumenPeticion{{Name: "datos", Mount: "d"}}},
		"mount raíz":     {Image: "base", Volumes: []VolumenPeticion{{Name: "datos", Mount: "/"}}},
		"nombre malo":    {Image: "base", Volumes: []VolumenPeticion{{Name: "../x", Mount: "/d"}}},
		"repetido":       {Image: "base", Volumes: []VolumenPeticion{{Name: "datos", Mount: "/d"}, {Name: "datos", Mount: "/e"}}},
		"clon en lote":   {Image: "base", Count: ptr(2), Volumes: []VolumenPeticion{{Name: "n", Mount: "/d", ReadOnly: true, CloneFrom: "datos"}}},
		"existe y clon":  {Image: "base", Volumes: []VolumenPeticion{{Name: "datos", Mount: "/d", CloneFrom: "otro"}}},
	}
	for n, p := range malos {
		code := 400
		if n == "existe y clon" {
			code = 409
		}
		if resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", p); resp.StatusCode != code {
			t.Errorf("%s: status %d, want %d", n, resp.StatusCode, code)
		}
	}
	if f.intentos != 0 {
		t.Fatal("invalid request reached the daemon")
	}
}

func TestVolumenesGestionYClonAlCrear(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxVolumenes: 3}, Tenant{Nombre: "bob", Token: tokenBob})
	resp := pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "base1", SizeMiB: 128, Host: "b"})
	esperaCodigo(t, resp, 201)
	v := decodifica[Volumen](t, resp)
	if v.Name != "base1" || v.Host != "b" || v.SizeMiB != 128 || !b.tieneVolumen(nombreReal(alice, "base1")) {
		t.Fatalf("created %+v", v)
	}
	// El del daemon lleva la etiqueta del dueño.
	if l := b.volumenes[nombreReal(alice, "base1")].Labels[LabelTenant]; l != "alice" {
		t.Fatalf("owner label %q", l)
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "base1"}), 409)
	// Bob puede llamar igual al suyo.
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "base1", Host: "a"}), 201)

	// Crear el sandbox con un clon al vuelo: nace en b, con el clon montado.
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base",
		Volumes: []VolumenPeticion{{Name: "copia", Mount: "/data", CloneFrom: "base1"}}})
	if sb.Host != "b" || !b.tieneVolumen(nombreReal(alice, "copia")) {
		t.Fatalf("host %q, clone present %v", sb.Host, b.tieneVolumen(nombreReal(alice, "copia")))
	}
	// El clon (sin etiquetas en el daemon) es de alice y se lista.
	l := decodifica[[]Volumen](t, pide(t, srv, tokenAlice, "GET", "/v1/volumes", nil))
	if len(l) != 2 {
		t.Fatalf("alice's volumes: %+v", l)
	}
	// Si el sandbox no se puede crear, el clon se deshace.
	b.mu.Lock()
	b.crearFallaDesde = 1
	b.mu.Unlock()
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base",
		Volumes: []VolumenPeticion{{Name: "otra", Mount: "/data", CloneFrom: "base1"}}}), 500)
	if b.tieneVolumen(nombreReal(alice, "otra")) {
		t.Fatal("clone left behind after a failed creation")
	}
	// Tope de volúmenes y borrado.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "tres"}), 201)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "cuatro"}), 429)
	esperaCodigo(t, pide(t, srv, tokenAlice, "DELETE", "/v1/volumes/tres", nil), 204)
	if b.tieneVolumen(nombreReal(alice, "tres")) || a.tieneVolumen(nombreReal(alice, "tres")) {
		t.Fatal("volume not deleted")
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/volumes/tres", nil), 404)
}
