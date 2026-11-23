package frontal

import (
	"strconv"
	"testing"
	"time"
)

// propiosDe cuenta las máquinas y volúmenes del falso que llevan la etiqueta
// del tenant nombre.
func propiosDe(f *falso, nombre string) (maquinas, volumenes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, mc := range f.maquinas {
		if mc.Labels[LabelTenant] == nombre {
			maquinas++
		}
	}
	for _, v := range f.volumenes {
		if v.Labels[LabelTenant] == nombre {
			volumenes++
		}
	}
	return
}

// Borrar un tenant con sandboxes o volúmenes vivos no puede dejarlos
// corriendo: con ellos, un tenant nuevo con el mismo nombre los heredaba, y
// sus puertos públicos seguían abiertos. Sin force se rechaza y no se toca
// nada; con force se destruyen y luego se borra.
func TestBorrarTenantConRecursos(t *testing.T) {
	b := montarTenants(t, "", nil, nil)
	k := b.tenantConClave(t, "carol", nil)
	crea(t, b.srv, k.Clave, CrearPeticion{Image: "base"})
	esperaCodigo(t, pide(t, b.srv, k.Clave, "POST", "/v1/volumes", CrearVolumenPeticion{Name: "datos", SizeMiB: 64}), 201)
	if m, v := propiosDe(b.f, "carol"); m != 1 || v != 1 {
		t.Fatalf("antes: %d máquinas, %d volúmenes", m, v)
	}

	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/carol", nil), 409)
	if m, v := propiosDe(b.f, "carol"); m != 1 || v != 1 {
		t.Errorf("el 409 tocó recursos: %d máquinas, %d volúmenes", m, v)
	}
	if c := codigoDe(t, b, k.Clave, "GET", "/v1/sandboxes"); c != 200 {
		t.Errorf("el 409 borró el tenant: %d", c)
	}

	esperaCodigo(t, pide(t, b.srv, tokenAdmin, "DELETE", "/v1/tenants/carol?force=true", nil), 204)
	if m, v := propiosDe(b.f, "carol"); m != 0 || v != 0 {
		t.Errorf("tras force quedan %d máquinas y %d volúmenes de carol", m, v)
	}
	if c := codigoDe(t, b, k.Clave, "GET", "/v1/sandboxes"); c != 401 {
		t.Errorf("tenant borrado: %d, want 401", c)
	}
}

// Un puerto público de un sandbox cuyo tenant ya no existe no se abre: antes
// bastaba con que la etiqueta de tenant no estuviera vacía.
func TestIngresoPublicoDeTenantBorrado(t *testing.T) {
	b := montarIngreso(t, nil)
	b.f.mu.Lock()
	b.sb.Labels[LabelTenant] = "fantasma"
	b.sb.Labels[LabelPublico] = "3000:" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	b.f.mu.Unlock()
	b.s.ingreso.olvidar(b.id)
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode == 200 {
		t.Fatalf("público de un tenant que no existe = %d", resp.StatusCode)
	}
}
