package guest

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// falsos sustituye las llamadas al sistema por grabadoras, y las repone al
// acabar: el manejador se prueba sin root y en cualquier sistema.
func falsos(t *testing.T, errReloj, errEntropia error) (*time.Time, *[]byte) {
	t.Helper()
	var reloj time.Time
	var ent []byte
	viejoReloj, viejaEnt := setClock, mixEntropy
	setClock = func(tm time.Time) error { reloj = tm; return errReloj }
	mixEntropy = func(b []byte) error { ent = append([]byte(nil), b...); return errEntropia }
	t.Cleanup(func() { setClock, mixEntropy = viejoReloj, viejaEnt })
	return &reloj, &ent
}

func postResync(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	ResyncHandler()(rr, httptest.NewRequest(http.MethodPost, api.GuestResyncPath, strings.NewReader(body)))
	return rr
}

func cuerpo(t *testing.T, nano int64, ent []byte) string {
	b, err := json.Marshal(api.GuestResync{UnixNano: nano, Entropy: ent})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResyncPoneRelojYEntropia(t *testing.T) {
	reloj, ent := falsos(t, nil, nil)
	host := time.Now().Add(7 * time.Minute)
	semilla := bytes.Repeat([]byte{0xAB}, api.GuestResyncEntropy)

	rr := postResync(t, cuerpo(t, host.UnixNano(), semilla))
	if rr.Code != http.StatusOK {
		t.Fatalf("código %d: %s", rr.Code, rr.Body)
	}
	if !reloj.Equal(host) {
		t.Fatalf("reloj = %v, quería %v", reloj, host)
	}
	if !bytes.Equal(*ent, semilla) {
		t.Fatalf("entropía = %x", *ent)
	}
	var res api.GuestResyncResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// El invitado "iba" 7 minutos atrasado: es el caso de un thaw.
	if res.SkewMS < 6*60*1000 || res.SkewMS > 8*60*1000 {
		t.Fatalf("skew_ms = %d, quería ~420000", res.SkewMS)
	}
}

func TestResyncRechazaLoQueNoCuadra(t *testing.T) {
	reloj, ent := falsos(t, nil, nil)
	ahora := time.Now().UnixNano()
	casos := map[string]struct {
		body string
		code int
	}{
		"no es JSON":         {"{", 400},
		"entropía corta":     {cuerpo(t, ahora, make([]byte, 31)), 400},
		"entropía larga":     {cuerpo(t, ahora, make([]byte, 513)), 400},
		"sin entropía":       {`{"unix_nano":1}`, 400},
		"hora cero":          {cuerpo(t, 0, make([]byte, 64)), 400},
		"hora de 1999":       {cuerpo(t, time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(), make([]byte, 64)), 400},
		"cuerpo desmesurado": {`{"entropy":"` + strings.Repeat("A", 5000) + `"}`, 413},
	}
	for nombre, c := range casos {
		if rr := postResync(t, c.body); rr.Code != c.code {
			t.Errorf("%s: código %d, quería %d (%s)", nombre, rr.Code, c.code, rr.Body)
		}
	}
	if !reloj.IsZero() || *ent != nil {
		t.Fatalf("una petición inválida llegó a tocar el sistema: reloj=%v ent=%x", reloj, *ent)
	}

	rr := httptest.NewRecorder()
	ResyncHandler()(rr, httptest.NewRequest(http.MethodGet, api.GuestResyncPath, nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rr.Code)
	}
}

// Si no se puede resembrar, el reloj ni se toca y el daemon ve un 500: la
// entropía es lo que importa y no se debe tapar su fallo.
func TestResyncFalloDeEntropia(t *testing.T) {
	reloj, _ := falsos(t, nil, errors.New("sin CAP_SYS_ADMIN"))
	rr := postResync(t, cuerpo(t, time.Now().UnixNano(), make([]byte, 64)))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "RNG") {
		t.Fatalf("código %d: %s", rr.Code, rr.Body)
	}
	if !reloj.IsZero() {
		t.Fatal("se puso el reloj sin haber resembrado")
	}
}

func TestResyncRegistradaEnElAgente(t *testing.T) {
	falsos(t, nil, nil)
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
	mux := http.NewServeMux()
	a.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, api.GuestResyncPath,
		strings.NewReader(cuerpo(t, time.Now().UnixNano(), make([]byte, 64)))))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /resync por el mux del agente: %d %s", rr.Code, rr.Body)
	}
}

func TestIsControlPath(t *testing.T) {
	for _, p := range []string{"/resync", "/resync/x", "/volume/release", "/volume", "/exec", "/exec/pty", "/files", "/dns"} {
		if !IsControlPath(p) {
			t.Errorf("%s debería ser de control", p)
		}
	}
	for _, p := range []string{"/", "/mcp", "/mcp/resync", "/resyncx", "/executor", "/healthz", "/sse"} {
		if IsControlPath(p) {
			t.Errorf("%s no debería ser de control", p)
		}
	}
}
