package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

func tresHerramientas() []api.ToolSpec {
	return []api.ToolSpec{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "c", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
}

// Si initialize falla, exerciseTools devolvia "" SIN error. El verify imprimia
// su ✓ igual, decia "exercised 3 tool(s)" y salia con codigo 0 sin haber llamado
// a ninguna. Una comprobacion que no puede fallar no comprueba nada.
func TestExerciseToolsFallaSiNoPudoNiEmpezar(t *testing.T) {
	post := func(sid, body string) (string, []byte, error) {
		return "", nil, errors.New("el invitado no contesta")
	}
	salida, ejercidas, err := exerciseTools(post, tresHerramientas())
	if err == nil {
		t.Fatal("initialize fallo y exerciseTools no lo dijo")
	}
	if ejercidas != 0 {
		t.Errorf("dice haber ejercido %d herramientas sin haber empezado", ejercidas)
	}
	if salida != "" {
		t.Errorf("salida inesperada: %q", salida)
	}
}

// Y solo se cuentan las que de verdad respondieron.
func TestExerciseToolsCuentaLasQueRespondieronNoLasQueHabia(t *testing.T) {
	llamadas := 0
	post := func(sid, body string) (string, []byte, error) {
		if strings.Contains(body, `"initialize"`) {
			return "sid", []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), nil
		}
		if !strings.Contains(body, `"tools/call"`) {
			return sid, []byte(`{}`), nil
		}
		llamadas++
		if llamadas == 2 {
			return "", nil, errors.New("timeout")
		}
		return sid, []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`), nil
	}

	_, ejercidas, err := exerciseTools(post, tresHerramientas())
	if err != nil {
		t.Fatalf("no deberia fallar: %v", err)
	}
	if ejercidas != 2 {
		t.Errorf("ejercidas = %d; respondieron 2 de 3, y decir 3 seria mentir", ejercidas)
	}
}

func TestExerciseToolsConTodoBienLasCuentaTodas(t *testing.T) {
	post := func(sid, body string) (string, []byte, error) {
		if strings.Contains(body, `"initialize"`) {
			return "sid", []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), nil
		}
		return sid, []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`), nil
	}
	_, ejercidas, err := exerciseTools(post, tresHerramientas())
	if err != nil || ejercidas != 3 {
		t.Errorf("ejercidas = %d, err = %v; esperaba 3 y nil", ejercidas, err)
	}
}
