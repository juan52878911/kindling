package main

import "testing"

// El mensaje de rechazo tiene que explicar el fallo QUE NO HA OCURRIDO TODAVIA.
//
// Congelar un servidor que no sirve produce un dorado que restaura bien y falla
// al despertar, minutos u horas despues, con un "tool did not start listening"
// que no menciona el commit. Quien lea el rechazo tiene que entender esa cadena
// sin haberla vivido, y saber como seguir.
func TestElRechazoDeCommitExplicaLaCadena(t *testing.T) {
	msg := mensajeNoSirve("det1", "30s")
	for _, quiero := range []string{
		"not serving",         // que pasa ahora
		"tool did not start",  // que pasaria despues
		"kling logs det1",     // como diagnosticar
		"kling commit -force", // como seguir si se quiere igual
		"30s",                 // cuanto se espero
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
