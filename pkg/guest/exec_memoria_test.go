package guest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// contador es un ResponseWriter que no guarda el cuerpo: el test mide lo que
// reserva el agente, no lo que reservaría un httptest.ResponseRecorder.
type contador struct {
	h   http.Header
	n   int
	ini []byte // los primeros bytes, para leer exit_code y truncated
	fin []byte // los últimos
}

func (c *contador) Header() http.Header {
	if c.h == nil {
		c.h = http.Header{}
	}
	return c.h
}
func (c *contador) WriteHeader(int) {}
func (c *contador) Write(p []byte) (int, error) {
	c.n += len(p)
	if len(c.ini) < 256 {
		c.ini = append(c.ini, p[:min(len(p), 256-len(c.ini))]...)
	}
	c.fin = append(c.fin, p...)
	if len(c.fin) > 256 {
		c.fin = append(c.fin[:0], c.fin[len(c.fin)-256:]...)
	}
	return len(p), nil
}

// Un /exec con mucha salida reserva poco más que la propia salida. Antes el
// bytes.Buffer crecía doblando, String() la copiaba y json la volvía a copiar
// escapada: 3-4× la salida en el pico, y en un invitado de 256 MiB eso con 64
// MiB de salida era el OOM de PID 1.
func TestExecSalidaGrandeSinCopias(t *testing.T) {
	const tam = 16 << 20
	body := `{"cmd":["sh","-c","head -c 16777216 /dev/zero | tr '\\0' a"]}`
	w := &contador{}
	runtime.GC()
	var antes, despues runtime.MemStats
	runtime.ReadMemStats(&antes)
	ExecHandler(nil)(w, httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(body)))
	runtime.ReadMemStats(&despues)
	if w.n < tam {
		t.Fatalf("response is %d bytes, want over %d: %q", w.n, tam, w.ini)
	}
	if !bytes.Contains(w.ini, []byte(`"exit_code":0`)) {
		t.Fatalf("response starts %q", w.ini)
	}
	reservado := despues.TotalAlloc - antes.TotalAlloc
	t.Logf("exec reserved %.1f MiB for %d MiB of output", float64(reservado)/(1<<20), tam>>20)
	if reservado > tam*3/2 {
		t.Fatalf("exec reserved %d MiB for %d MiB of output", reservado>>20, tam>>20)
	}
}

// La respuesta en streaming dice lo mismo que la de encoding/json para
// cualquier salida: UTF-8 inválido (también partido entre bloques), comillas,
// controles, <>& y U+2028.
func TestEscribirJSONStringIgualQueEncodingJSON(t *testing.T) {
	casos := []string{"", "hola\n", "\"\\\x00\x1f\x7f<>&  \t\r\b\f", "\xff\xfe a \xc3", "ñ€😀",
		strings.Repeat("ñ", 40000), strings.Repeat("\xe2\x82", 50000)}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		b := make([]byte, r.Intn(300000))
		r.Read(b)
		casos = append(casos, string(b))
	}
	for i, s := range casos {
		out := newCappedBuffer(len(s))
		// Escrito en trozos impares: los bloques no caen en el borde de una runa.
		for p := []byte(s); len(p) > 0; {
			n := min(len(p), 1+r.Intn(70000))
			out.Write(p[:n])
			p = p[n:]
		}
		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := out.writeJSONString(bw); err != nil {
			t.Fatal(err)
		}
		bw.Flush()
		var got, want string
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("case %d: not JSON: %v", i, err)
		}
		ref, _ := json.Marshal(s)
		json.Unmarshal(ref, &want)
		if got != want {
			t.Fatalf("case %d: decoded %d bytes, encoding/json %d", i, len(got), len(want))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("case %d: invalid UTF-8", i)
		}
	}
}

// Tras una salida grande se devuelve la memoria al sistema (y al globo); con
// una pequeña, no: FreeOSMemory es una GC entera.
func TestExecLiberaTrasSalidaGrande(t *testing.T) {
	viejo := liberarMemoria
	defer func() { liberarMemoria = viejo }()
	veces := 0
	liberarMemoria = func() { veces++ }
	for _, c := range []struct {
		cmd  string
		want int
	}{{`{"cmd":["echo","ok"]}`, 0}, {`{"cmd":["sh","-c","head -c 8388608 /dev/zero"]}`, 1}} {
		veces = 0
		ExecHandler(nil)(&contador{}, httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(c.cmd)))
		if veces != c.want {
			t.Errorf("%s: freed %d times, want %d", c.cmd, veces, c.want)
		}
	}
}
