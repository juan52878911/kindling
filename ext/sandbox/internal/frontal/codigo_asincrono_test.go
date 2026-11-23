package frontal

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// R16: un rechazo sin manejar o un throw en un temporizador no matan el
// conductor, se ven como stderr de la ejecución que los causó y la salida
// tardía de una ejecución cerrada no aparece en la siguiente.
func TestRevJSAsincronoSuelto(t *testing.T) {
	sinHerramienta(t, "node")
	esperaMs := "new Promise(r => setTimeout(r, 300))"

	t.Run("rechazo sin manejar", func(t *testing.T) {
		b := montarCodigo(t)
		id := b.contextoDe(t, "javascript")
		ejecutaOK(t, b.corre(t, id, "var n = 5; 0"))
		evs := b.corre(t, id, "setTimeout(() => { Promise.reject(new Error('rechazo-suelto')); }, 20); "+esperaMs)
		final(t, evs)
		if !strings.Contains(texto(evs, "stderr"), "rechazo-suelto") {
			t.Fatalf("el rechazo no salió como stderr: %+v", evs)
		}
		evs = b.corre(t, id, "n")
		ejecutaOK(t, evs)
		if texto(evs, "result") != "5" {
			t.Fatalf("el contexto perdió su estado: %+v", evs)
		}
	})

	t.Run("throw en un temporizador", func(t *testing.T) {
		b := montarCodigo(t)
		id := b.contextoDe(t, "javascript")
		ejecutaOK(t, b.corre(t, id, "var n = 7; 0"))
		evs := b.corre(t, id, "setTimeout(() => { throw new Error('lanzado-tarde'); }, 20); "+esperaMs)
		final(t, evs)
		if !strings.Contains(texto(evs, "stderr"), "lanzado-tarde") {
			t.Fatalf("el throw no salió como stderr: %+v", evs)
		}
		evs = b.corre(t, id, "n")
		ejecutaOK(t, evs)
		if texto(evs, "result") != "7" {
			t.Fatalf("el contexto murió: %+v", evs)
		}
	})

	t.Run("salida tardía no pasa a la siguiente", func(t *testing.T) {
		b := montarCodigo(t)
		id := b.contextoDe(t, "javascript")
		ejecutaOK(t, b.corre(t, id, "setTimeout(() => { console.log('tardia-ajena'); }, 150); 1"))
		evs := b.corre(t, id, esperaMs)
		final(t, evs)
		if s := texto(evs, "stdout") + texto(evs, "stderr"); strings.Contains(s, "tardia-ajena") {
			t.Fatalf("salida de la ejecución anterior en esta: %+v", evs)
		}
	})
}

// R17: buscar en una raíz que no existe es 404, no un 200 vacío.
func TestRevBuscarRutaInexistente(t *testing.T) {
	b := montarCodigo(t)
	dir := t.TempDir()
	resp := pide(t, b.srv, tokenAlice, "GET", b.base+"/files/search?path="+dir+"/no-existe&glob=*.log", nil)
	esperaCodigo(t, resp, http.StatusNotFound)
	if e := decodifica[api.Error](t, resp); e.Code == "" {
		t.Errorf("sin code: %+v", e)
	}
}

// R17: un Range que empieza más allá de lo descargable es 416.
func TestRevRangePasaDelTope(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	base := "/v1/sandboxes/" + sb.ID
	esperaCodigo(t, pide(t, srv, tokenAlice, "PUT", base+"/files?path=/tmp/g.bin", "x"), 200)
	f.mu.Lock()
	f.tamanoStat = api.FileMaxDownload + 100
	f.mu.Unlock()
	for _, rango := range []string{"bytes=" + "268435456-", "bytes=-50"} {
		req := peticion(t, srv, tokenAlice, "GET", base+"/files?path=/tmp/g.bin", nil)
		req.Header.Set("Range", rango)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 416 {
			t.Errorf("%s: %d, quería 416", rango, resp.StatusCode)
		}
	}
}
