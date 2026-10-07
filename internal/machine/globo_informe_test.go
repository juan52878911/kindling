package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// cuerposBalloon son los PUT /balloon que recibió el VMM falso, decodificados.
func cuerposBalloon(t *testing.T, f *fcFalso) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range f.llamadasA(http.MethodPut, "/balloon") {
		var b map[string]any
		if err := json.Unmarshal(l.Cuerpo, &b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// En Firecracker el globo pide el informe de páginas libres; con
// KLING_FREE_PAGE_REPORTING=0, no.
func TestGloboPideInformeDePaginasLibres(t *testing.T) {
	for _, apagado := range []bool{false, true} {
		if apagado {
			t.Setenv("KLING_FREE_PAGE_REPORTING", "0")
		}
		f := nuevoFcFalso(t)
		m := &Manager{}
		if err := m.configurarGlobo(context.Background(), f.cliente(), "abcdef0123456789", 256); err != nil {
			t.Fatal(err)
		}
		cs := cuerposBalloon(t, f)
		if len(cs) != 1 {
			t.Fatalf("PUT /balloon %d veces", len(cs))
		}
		quiero := !apagado && !globoSinEstadisticas
		if got, _ := cs[0]["free_page_reporting"].(bool); got != quiero {
			t.Errorf("apagado=%v: free_page_reporting=%v, quería %v", apagado, cs[0]["free_page_reporting"], quiero)
		}
		if cs[0]["amount_mib"] != float64(256) || cs[0]["deflate_on_oom"] != true {
			t.Errorf("el resto del globo cambió: %v", cs[0])
		}
	}
}

// Un Firecracker que no conoce el campo lo rechaza: se pide otra vez sin él y
// la máquina arranca con el globo de siempre.
func TestGloboSinInformeEnUnVMMViejo(t *testing.T) {
	if !informePaginasLibres() {
		t.Skip("sin informe de páginas libres en esta plataforma")
	}
	f := nuevoFcFalso(t)
	f.fallar(http.MethodPut, "/balloon", http.StatusBadRequest, "unknown field `free_page_reporting`")
	f.enGancho(func(metodo, ruta string) {
		if metodo == http.MethodPut && ruta == "/balloon" {
			f.dejarDeFallar(metodo, ruta) // solo la primera
		}
	})
	m := &Manager{}
	if err := m.configurarGlobo(context.Background(), f.cliente(), "abcdef0123456789", 0); err != nil {
		t.Fatalf("tenía que reintentar sin el informe: %v", err)
	}
	cs := cuerposBalloon(t, f)
	if len(cs) != 2 || cs[0]["free_page_reporting"] != true || cs[1]["free_page_reporting"] != nil {
		t.Fatalf("pedidos: %v", cs)
	}
}
