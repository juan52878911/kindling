package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Un código de salida distinto de cero NO es un error HTTP.
//
// La petición se atendió perfectamente; la respuesta es "el comando falló, aquí
// tienes por qué". Devolver 500 obligaría a quien llama a distinguir "no pude
// ejecutarlo" de "lo ejecuté y falló", que son cosas muy distintas: la primera
// se reintenta, la segunda no.
func TestUnComandoQueFallaNoEsUnErrorHTTP(t *testing.T) {
	b := &bridge{}
	body := `{"cmd":["sh","-c","echo antes; echo el motivo >&2; exit 3"]}`
	w := httptest.NewRecorder()
	b.handleExec(w, httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("código HTTP = %d, want 200", w.Code)
	}
	var res execResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", res.ExitCode)
	}
	// Las dos salidas, ENTRELAZADAS: un install que peta escribe el motivo en
	// stderr entre líneas de progreso de stdout, y separarlas lo vuelve ilegible.
	if !strings.Contains(res.Output, "antes") || !strings.Contains(res.Output, "el motivo") {
		t.Errorf("perdió una de las dos salidas: %q", res.Output)
	}
}

// Si el binario no existe, hay que DECIRLO: no habrá salida que lo explique.
func TestUnBinarioQueNoExisteLoDice(t *testing.T) {
	b := &bridge{}
	w := httptest.NewRecorder()
	b.handleExec(w, httptest.NewRequest(http.MethodPost, "/exec",
		strings.NewReader(`{"cmd":["no-existe-este-binario-jamas"]}`)))

	var res execResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", res.ExitCode)
	}
	if !strings.Contains(res.Output, "no pude ejecutar") {
		t.Errorf("no explica que ni se pudo lanzar: %q", res.Output)
	}
}

// Peticiones mal formadas se rechazan sin ejecutar nada.
func TestExecRechazaLoQueNoEntiende(t *testing.T) {
	b := &bridge{}
	casos := []struct {
		nombre string
		metodo string
		cuerpo string
		want   int
	}{
		{"GET no vale", http.MethodGet, "", http.StatusMethodNotAllowed},
		{"cuerpo que no es JSON", http.MethodPost, "esto no", http.StatusBadRequest},
		{"sin comando", http.MethodPost, `{"cmd":[]}`, http.StatusBadRequest},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			w := httptest.NewRecorder()
			b.handleExec(w, httptest.NewRequest(c.metodo, "/exec", strings.NewReader(c.cuerpo)))
			if w.Code != c.want {
				t.Errorf("código = %d, want %d", w.Code, c.want)
			}
		})
	}
}

// La capacidad de ejecutar comandos la concede el ANFITRIÓN, por la línea de
// comandos del kernel. El invitado no puede dársela a sí mismo.
//
// Importa porque el gateway reenvía peticiones a los invitados: una ruta que
// ejecuta comandos y solo se protege por no ser alcanzable es una ruta que
// alguien acaba alcanzando. En una microVM de servicio ni siquiera está
// registrada.
func TestLaEjecucionSoloLaEnciendeElKernel(t *testing.T) {
	// Sin /proc/cmdline legible (macOS, o un cmdline sin el parámetro) queda
	// apagada, que es el valor seguro por defecto.
	if execEnabled() {
		t.Error("la ejecución debería estar apagada si el kernel no la pide")
	}
	if execBootParam != "kling.exec" {
		t.Errorf("el parámetro cambió de nombre: %q", execBootParam)
	}
}
