package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El mensaje de rechazo tiene que explicar el fallo QUE NO HA OCURRIDO TODAVIA.
//
// Congelar un servidor que no sirve produce un dorado que restaura bien y falla
// al despertar, minutos u horas despues, con un "tool did not start listening"
// que no menciona el commit. Quien lea el rechazo tiene que entender esa cadena
// sin haberla vivido, y saber como seguir.
func TestElRechazoDeCommitExplicaLaCadena(t *testing.T) {
	msg := mensajeNoSirve("det1", "30s")
	for _, quiero := range []string{
		"not serving",        // que pasa ahora
		"tool did not start", // que pasaria despues
		"kling logs det1",    // como diagnosticar
		"kling save -force",  // como seguir si se quiere igual
		"30s",                // cuanto se espero
	} {
		if !contiene(msg, quiero) {
			t.Errorf("el rechazo no menciona %q:\n%s", quiero, msg)
		}
	}
}

func contiene(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// Una máquina que no existe no es un invitado que "no sirve tras 2m0s": el
// rechazo dice solo eso, sin el plazo ni el puerto.
func TestSaveDeUnaMaquinaQueNoExiste(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/{ref}/guest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"that machine doesn't exist"}`))
	})
	c := fakeDaemon(t, mux)
	err := listoParaCongelar(context.Background(), c, "nadie", 2*time.Minute, true)
	if err == nil || !api.IsNotFound(err) || strings.Contains(err.Error(), "not serving") || strings.Contains(err.Error(), "2m0s") {
		t.Fatalf("save de una que no existe = %v", err)
	}
}
