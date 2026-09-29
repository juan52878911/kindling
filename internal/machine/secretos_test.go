package machine

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
)

func TestAlmacenVacio(t *testing.T) {
	for _, v := range []any{json.RawMessage(`{}`), json.RawMessage(` { } `), json.RawMessage(`null`), nil} {
		if !almacenVacio(v) {
			t.Errorf("%s tiene que contar como vacío", v)
		}
	}
	for _, v := range []any{json.RawMessage(`{"phone":{}}`), json.RawMessage(`{"env":{}}`), json.RawMessage(`[]`)} {
		if almacenVacio(v) {
			t.Errorf("%s no está vacío", v)
		}
	}
}

// La identidad por copia: secreto → ganchos que lo consumen → almacén vacío
// levanta la marca. Sin ganchos confirmados en medio, no.
func TestSecretoSeLevantaTrasGanchosYVaciar(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	a := &agenteListo{ganchos: true, listoEn: 1}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	id := "abcdef0123456789"
	mc := m.addForTest(id)
	falso := nuevoFcFalso(t)
	m.mu.Lock()
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(srv.URL, "http://")}
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	ctx := context.Background()
	secreto := json.RawMessage(`{"phone":{"android_id":"a1b2"}}`)
	vacio := json.RawMessage(`{}`)

	// Vaciar sin ganchos en medio: sigue marcada.
	if out, err := m.PutMMDS(ctx, id, secreto); err != nil || !out.HasSecrets {
		t.Fatalf("inyectar = %+v, %v", out, err)
	}
	if out, _ := m.PutMMDS(ctx, id, vacio); !out.HasSecrets {
		t.Fatal("sin ganchos que lo consuman, vaciar no levanta la marca")
	}
	// Ganchos que terminan bien, y luego vaciar: se levanta.
	if _, err := m.RunHooks(ctx, id, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if out, _ := m.PutMMDS(ctx, id, vacio); out.HasSecrets {
		t.Fatal("tras ganchos confirmados y almacén vacío, la marca se levanta")
	}
	// Una inyección DESPUÉS de los ganchos invalida la confirmación.
	m.PutMMDS(ctx, id, secreto)
	m.RunHooks(ctx, id, 5*time.Second)
	m.PutMMDS(ctx, id, secreto)
	if out, _ := m.PutMMDS(ctx, id, vacio); !out.HasSecrets {
		t.Fatal("un secreto nuevo tras los ganchos no está consumido")
	}
	// Ganchos que fallan no confirman nada.
	a.mu.Lock()
	a.falla = true
	a.mu.Unlock()
	m.PutMMDS(ctx, id, secreto)
	if _, err := m.RunHooks(ctx, id, 5*time.Second); err == nil {
		t.Fatal("ganchos fallidos tienen que dar error")
	}
	if out, _ := m.PutMMDS(ctx, id, vacio); !out.HasSecrets {
		t.Fatal("ganchos fallidos no levantan la marca")
	}
	// Vaciar una máquina que nunca tuvo secretos no la marca.
	otra := "0123456789abcdef"
	m.addForTest(otra)
	m.mu.Lock()
	m.socket[otra] = falso.Sock
	m.mu.Unlock()
	if out, _ := m.PutMMDS(ctx, otra, vacio); out.HasSecrets {
		t.Fatal("un almacén vacío no es un secreto")
	}
}
