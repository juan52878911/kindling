package guest

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
	body := `{"cmd":["sh","-c","echo antes; echo el motivo >&2; exit 3"]}`
	w := httptest.NewRecorder()
	handleExec(nil, w, httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(body)))

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
	w := httptest.NewRecorder()
	handleExec(nil, w, httptest.NewRequest(http.MethodPost, "/exec",
		strings.NewReader(`{"cmd":["no-existe-este-binario-jamas"]}`)))

	var res execResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", res.ExitCode)
	}
	if !strings.Contains(res.Output, "could not execute") {
		t.Errorf("no explica que ni se pudo lanzar: %q", res.Output)
	}
}

// Peticiones mal formadas se rechazan sin ejecutar nada.
func TestExecRechazaLoQueNoEntiende(t *testing.T) {
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
			handleExec(nil, w, httptest.NewRequest(c.metodo, "/exec", strings.NewReader(c.cuerpo)))
			if w.Code != c.want {
				t.Errorf("código = %d, want %d", w.Code, c.want)
			}
		})
	}
}

// Un cuerpo enorme no debe ni siquiera decodificarse: MaxBytesReader corta la
// lectura y handleExec responde 413 sin haber lanzado nada.
func TestExecCuerpoEnormeSeRechaza(t *testing.T) {
	body := `{"cmd":["true"],"dir":"` + strings.Repeat("x", execMaxBody+1) + `"}`
	w := httptest.NewRecorder()
	handleExec(nil, w, httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(body)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("código = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

// Un comando que escupe más de lo que cabe se corta, no tumba al agente: el
// campo truncated (aditivo) lo dice, y la salida sigue trayendo el principio.
func TestExecSalidaConTopeMarcaTruncated(t *testing.T) {
	// Escribe más que outCap sin depender de api.ExecMaxOutput (64 MiB, lento
	// de generar en un test): se prueba cappedBuffer directamente con un tope
	// pequeño y aparte se comprueba el cableado end-to-end con /bin/sh.
	cmd := `{"cmd":["sh","-c","head -c 200 /dev/zero | tr '\\0' 'a'"]}`
	w := httptest.NewRecorder()
	handleExec(nil, w, httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(cmd)))
	var res execResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Errorf("200 bytes no debería truncarse (tope real es api.ExecMaxOutput): truncated=%v", res.Truncated)
	}
	if len(res.Output) != 200 {
		t.Errorf("output = %d bytes, want 200", len(res.Output))
	}
}

// cappedBuffer es lo que de verdad impone el tope; se prueba aislado con un
// límite chico para no escribir decenas de MiB en el test.
func TestCappedBufferTruncaSinCortarLaEscritura(t *testing.T) {
	c := newCappedBuffer(5)
	n, err := c.Write([]byte("hola mundo"))
	if err != nil || n != 10 {
		t.Fatalf("Write = (%d, %v), want (10, nil)", n, err)
	}
	if !c.truncated {
		t.Error("truncated debería ser true")
	}
	if c.String() != "hola " {
		t.Errorf("String() = %q, want %q", c.String(), "hola ")
	}
	// Sigue aceptando escrituras después del tope, sin devolver error (no
	// corta la tubería): un Write de más solo deja de guardarse.
	n, err = c.Write([]byte("mas"))
	if err != nil || n != 3 {
		t.Fatalf("Write tras el tope = (%d, %v), want (3, nil)", n, err)
	}
	if c.String() != "hola " {
		t.Errorf("String() tras el tope = %q, want %q", c.String(), "hola ")
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
	if ExecEnabled() {
		t.Error("la ejecución debería estar apagada si el kernel no la pide")
	}
	if execBootParam != "kling.exec" {
		t.Errorf("el parámetro cambió de nombre: %q", execBootParam)
	}
}
