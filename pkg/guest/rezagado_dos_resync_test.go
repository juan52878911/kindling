package guest

import (
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// R6-I2 (R3): el segundo tiempo de la generación 1 se cuelga y vence su tope;
// llega OTRO resync (generación 2), que acaba bien. El rezagado de la 1 no
// puede, al destrabarse, publicar la generación 1 encima de la 2 ni renovar la
// identidad: la marca de vencido tiene que ser por generación.
func TestR6I2RezagadoTrasOtroResyncPublicaGeneracionVieja(t *testing.T) {
	falsos(t, nil, nil)
	ri, tope := renovarIdentidad, topeSegundoTiempo
	suelta := make(chan struct{})
	var llamadas atomic.Int32
	renovarIdentidad = func() []string {
		if llamadas.Add(1) == 1 {
			<-suelta // la E/S del primer segundo tiempo, colgada
		}
		return nil
	}
	topeSegundoTiempo = 150 * time.Millisecond
	t.Cleanup(func() { renovarIdentidad, topeSegundoTiempo = ri, tope })

	esperaPuerta := func(que string) {
		t.Helper()
		fin := time.Now().Add(3 * time.Second)
		for puertaResync.Cerrada() {
			if time.Now().After(fin) {
				t.Fatalf("la puerta no se abrió (%s)", que)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	leeGen := func() string {
		b, _ := os.ReadFile(generationFile)
		return strings.TrimSpace(string(b))
	}

	if rr := postResync(t, cuerpoInstancia(t)); rr.Code != http.StatusOK {
		t.Fatalf("resync 1: %d %s", rr.Code, rr.Body)
	}
	esperaPuerta("tope del primero")
	if rr := postResync(t, cuerpoInstancia(t)); rr.Code != http.StatusOK {
		t.Fatalf("resync 2: %d %s", rr.Code, rr.Body)
	}
	fin := time.Now().Add(3 * time.Second)
	for leeGen() != "2" {
		if time.Now().After(fin) {
			t.Fatalf("el segundo resync no publicó su generación: %q", leeGen())
		}
		time.Sleep(5 * time.Millisecond)
	}
	esperaPuerta("segundo resync")

	close(suelta) // el rezagado de la generación 1 se destraba
	segundosVivos.Wait()
	if g := leeGen(); g != "2" {
		t.Fatalf("el segundo tiempo rezagado de la generación 1 publicó %q encima de la 2", g)
	}
}
