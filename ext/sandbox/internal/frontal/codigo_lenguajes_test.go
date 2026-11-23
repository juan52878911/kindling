package frontal

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func (b *bancoCodigo) contextoDe(t *testing.T, lengua string) string {
	t.Helper()
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code/contexts", map[string]any{"language": lengua})
	esperaCodigo(t, resp, 201)
	c := decodifica[Contexto](t, resp)
	if c.ID == "" || c.Language != lengua {
		t.Fatalf("context: %+v", c)
	}
	return c.ID
}

func sinHerramienta(t *testing.T, n string) {
	t.Helper()
	if _, err := exec.LookPath(n); err != nil {
		t.Skipf("no %s in this runner", n)
	}
}

func final(t *testing.T, evs []evCodigo) evCodigo {
	t.Helper()
	if len(evs) == 0 || evs[len(evs)-1].Type != "done" {
		t.Fatalf("the stream does not end in done: %+v", evs)
	}
	return evs[len(evs)-1]
}

func ejecutaOK(t *testing.T, evs []evCodigo) {
	t.Helper()
	if f := final(t, evs); f.OK == nil || !*f.OK {
		t.Fatalf("not ok: %+v", evs)
	}
}

func hayError(evs []evCodigo, ename string) bool {
	for _, e := range evs {
		if e.Type == "error" && e.Ename == ename {
			return true
		}
	}
	return false
}

// results por MIME y execution_count en Python.
func TestCodigoResultsPorMIME(t *testing.T) {
	b := montarCodigo(t)
	id := b.contexto(t)

	evs := b.corre(t, id, "1 + 1")
	ejecutaOK(t, evs)
	var res *evCodigo
	for i := range evs {
		if evs[i].Type == "result" {
			res = &evs[i]
		}
	}
	if res == nil || res.Results["text/plain"] != "2" || res.Text != "2" || res.ExecutionCount != 1 || len(res.Results) != 1 {
		t.Fatalf("result: %+v", evs)
	}
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNGfalso"))
	b.corre(t, id, "class R:\n    def _repr_html_(self): return '<b>x</b>'\n    def _repr_png_(self): return b'\\x89PNGfalso'\n    def __repr__(self): return 'R()'\n")
	evs = b.corre(t, id, "R()")
	res = nil
	for i := range evs {
		if evs[i].Type == "result" {
			res = &evs[i]
		}
	}
	if res == nil || res.Results["text/plain"] != "R()" || res.Results["text/html"] != "<b>x</b>" ||
		res.Results["image/png"] != png || res.ExecutionCount != 3 {
		t.Fatalf("rich result: %+v", evs)
	}
	if f := final(t, evs); f.ExecutionCount != 3 {
		t.Fatalf("done without the count: %+v", f)
	}
}

func TestCodigoBash(t *testing.T) {
	sinHerramienta(t, "bash")
	// El conductor de bash usa bash 4 o posterior (el de las imágenes Linux);
	// el /bin/bash 3.2 de macOS no guarda el estado entre celdas.
	if v, err := exec.Command("bash", "-c", "echo ${BASH_VERSINFO[0]}").Output(); err != nil || strings.TrimSpace(string(v)) < "4" {
		t.Skipf("hace falta bash >= 4 (hay %q)", strings.TrimSpace(string(v)))
	}
	b := montarCodigo(t)
	id := b.contextoDe(t, "bash")

	ejecutaOK(t, b.corre(t, id, "x=5\nfunc() { echo \"f$1\"; }\ncd /tmp\nexport Y=yy"))
	evs := b.corre(t, id, "echo \"x=$x y=$Y\"; func 1; pwd; echo err >&2; echo 'q\"t\\ab'")
	ejecutaOK(t, evs)
	if got := texto(evs, "stdout"); got != "x=5 y=yy\nf1\n/tmp\nq\"t\\ab\n" {
		t.Fatalf("stdout %q (%+v)", got, evs)
	}
	if got := texto(evs, "stderr"); got != "err\n" {
		t.Fatalf("stderr %q", got)
	}
	// Un exit no mata el contexto; un estado distinto de 0 es un error.
	evs = b.corre(t, id, "echo antes; exit 3")
	if f := final(t, evs); f.OK == nil || *f.OK || !hayError(evs, "ExitStatus") || texto(evs, "stdout") != "antes\n" {
		t.Fatalf("exit 3: %+v", evs)
	}
	evs = b.corre(t, id, "echo $x")
	ejecutaOK(t, evs)
	if texto(evs, "stdout") != "5\n" {
		t.Fatalf("el estado se perdió tras exit: %+v", evs)
	}
	// Plazo: se corta y avisa.
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": "sleep 30", "context_id": id, "timeout_seconds": 1})
	esperaCodigo(t, resp, 200)
	evs = leerEventos(t, resp)
	if f := final(t, evs); f.OK == nil || *f.OK || !hayError(evs, "TimeoutError") {
		t.Fatalf("timeout: %+v", evs)
	}
	// Efímero (sin contexto): también va.
	evs = b.correLengua(t, "bash", "echo suelto")
	ejecutaOK(t, evs)
	if texto(evs, "stdout") != "suelto\n" {
		t.Fatalf("efímero: %+v", evs)
	}
}

func TestCodigoJavaScript(t *testing.T) {
	sinHerramienta(t, "node")
	b := montarCodigo(t)
	id := b.contextoDe(t, "javascript")

	evs := b.corre(t, id, "let a = 2; var o = {k: 1}; console.log('hi', o); console.error('mal')")
	ejecutaOK(t, evs)
	if texto(evs, "stdout") != "hi { k: 1 }\n" || texto(evs, "stderr") != "mal\n" {
		t.Fatalf("console: %+v", evs)
	}
	evs = b.corre(t, id, "a * 21")
	ejecutaOK(t, evs)
	var res *evCodigo
	for i := range evs {
		if evs[i].Type == "result" {
			res = &evs[i]
		}
	}
	if res == nil || res.Results["text/plain"] != "42" || res.ExecutionCount != 2 {
		t.Fatalf("result: %+v", evs)
	}
	evs = b.corre(t, id, "Promise.resolve(7)")
	res = nil
	for i := range evs {
		if evs[i].Type == "result" {
			res = &evs[i]
		}
	}
	if res == nil || res.Results["text/plain"] != "7" {
		t.Fatalf("promesa: %+v", evs)
	}
	evs = b.corre(t, id, "throw new TypeError('malo')")
	if f := final(t, evs); f.OK == nil || *f.OK || !hayError(evs, "TypeError") {
		t.Fatalf("error: %+v", evs)
	}
	ejecutaOK(t, b.corre(t, id, "a + 1"))
}

func leerEventos(t *testing.T, resp *http.Response) []evCodigo {
	t.Helper()
	defer resp.Body.Close()
	var evs []evCodigo
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var e evCodigo
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		evs = append(evs, e)
	}
	return evs
}

func (b *bancoCodigo) correLengua(t *testing.T, lengua, codigo string) []evCodigo {
	t.Helper()
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": codigo, "language": lengua})
	esperaCodigo(t, resp, 200)
	defer resp.Body.Close()
	var evs []evCodigo
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var e evCodigo
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		evs = append(evs, e)
	}
	return evs
}

// DELETE .../execution interrumpe la ejecución en curso y el contexto sigue.
func TestCodigoInterrumpir(t *testing.T) {
	casos := []struct {
		lengua, herramienta, bucle, ok string
		ename                          string
	}{
		{"python", "python3", "import time\ntime.sleep(30)", "1 + 1", "KeyboardInterrupt"},
		{"bash", "bash", "sleep 30", "echo vivo", "KeyboardInterrupt"},
		{"javascript", "node", "while (true) {}", "1 + 1", "KeyboardInterrupt"},
	}
	for _, c := range casos {
		t.Run(c.lengua, func(t *testing.T) {
			sinHerramienta(t, c.herramienta)
			b := montarCodigo(t)
			id := b.contextoDe(t, c.lengua)
			var wg sync.WaitGroup
			var evs []evCodigo
			wg.Add(1)
			go func() {
				defer wg.Done()
				evs = b.corre(t, id, c.bucle)
			}()
			// Hasta que la ejecución está en marcha, el DELETE no tiene a quién interrumpir.
			ruta := b.base + "/code/contexts/" + id + "/execution"
			time.Sleep(700 * time.Millisecond)
			resp := pide(t, b.srv, tokenAlice, "DELETE", ruta, nil)
			esperaCodigo(t, resp, 204)
			resp.Body.Close()
			hecho := make(chan struct{})
			go func() { wg.Wait(); close(hecho) }()
			select {
			case <-hecho:
			case <-time.After(15 * time.Second):
				t.Fatal("la ejecución no se interrumpió")
			}
			if f := final(t, evs); f.OK == nil || *f.OK || !hayError(evs, c.ename) {
				t.Fatalf("interrumpida: %+v", evs)
			}
			ejecutaOK(t, b.corre(t, id, c.ok))
			// Otro id: 404.
			esperaCodigo(t, pide(t, b.srv, tokenAlice, "DELETE", b.base+"/code/contexts/aaaaaaaaaaaa/execution", nil), 404)
			esperaCodigo(t, pide(t, b.srv, tokenBob, "DELETE", ruta, nil), 404)
		})
	}
}

func TestCodigoLenguajes(t *testing.T) {
	b := montarCodigo(t)
	for _, c := range []struct {
		ruta   string
		cuerpo map[string]any
	}{
		{"/code/contexts", map[string]any{"language": "ruby"}},
		{"/code", map[string]any{"code": "1", "language": "ruby"}},
	} {
		resp := pide(t, b.srv, tokenAlice, "POST", b.base+c.ruta, c.cuerpo)
		esperaCodigo(t, resp, 400)
		if e := decodifica[api.Error](t, resp); !strings.Contains(e.Message, "ruby") {
			t.Errorf("%s: %q", c.ruta, e.Message)
		}
	}
	// El lenguaje de la petición no puede contradecir el del contexto.
	id := b.contexto(t)
	resp := pide(t, b.srv, tokenAlice, "POST", b.base+"/code", map[string]any{"code": "echo 1", "language": "bash", "context_id": id})
	esperaCodigo(t, resp, 400)
	resp.Body.Close()

	// Sin node en la imagen: 422 que lo dice.
	b.d.mu.Lock()
	b.d.sinInterprete = "node"
	b.d.mu.Unlock()
	resp = pide(t, b.srv, tokenAlice, "POST", b.base+"/code/contexts", map[string]any{"language": "javascript"})
	esperaCodigo(t, resp, 422)
	if e := decodifica[api.Error](t, resp); !strings.Contains(e.Message, "node") {
		t.Errorf("message %q doesn't say node", e.Message)
	}
}
