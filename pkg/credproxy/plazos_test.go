package credproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// Los plazos de verdad son minutos; aquí se bajan a cientos de milisegundos
// con los campos del Proxy para que los tests prueben lo mismo en un segundo.

func credsUna() []Credential {
	return []Credential{{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret}}
}

func conPlazos(idle, max time.Duration) func(*Proxy) {
	return func(p *Proxy) { p.idle, p.max = idle, max }
}

func pedirStream(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/stream", nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	return http.DefaultClient.Do(req)
}

// Un stream que dura bastante más que el plazo de inactividad, pero que no
// para de moverse, llega entero. Con el plazo TOTAL de antes (aquí lo haría
// cualquier plazo menor que la duración del stream) se cortaba a mitad.
func TestProxyStreamLargoQueNoParaLlegaEntero(t *testing.T) {
	const eventos = 8
	srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < eventos; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}, credsUna(), conPlazos(400*time.Millisecond, 10*time.Second))

	t0 := time.Now()
	resp, err := pedirStream(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	dur := time.Since(t0)
	if err != nil {
		t.Fatalf("el stream se cortó tras %v: %v (%q)", dur, err, body)
	}
	if n := strings.Count(string(body), "data: "); n != eventos {
		t.Fatalf("llegaron %d de %d eventos: %q", n, eventos, body)
	}
	if dur < 3*400*time.Millisecond {
		t.Fatalf("el stream duró %v: el test no prueba nada si no pasa de varias veces el plazo de inactividad", dur)
	}
}

// Un proveedor que se queda callado a mitad se corta por inactividad, y el
// invitado ve un error, no una respuesta que parece completa.
func TestProxyCortaUnStreamParadoPorInactividad(t *testing.T) {
	fin := make(chan struct{})
	t.Cleanup(func() { close(fin) })
	srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: uno\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-fin:
		case <-r.Context().Done():
		}
	}, credsUna(), conPlazos(300*time.Millisecond, 10*time.Second))

	t0 := time.Now()
	resp, err := pedirStream(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	dur := time.Since(t0)
	if err == nil {
		t.Fatalf("el corte por inactividad debería verse como error, llegó %q entero", body)
	}
	if !strings.Contains(string(body), "data: uno") {
		t.Errorf("el primer evento debería haber llegado: %q", body)
	}
	if dur < 300*time.Millisecond || dur > 3*time.Second {
		t.Errorf("cortado a los %v; quería ~300ms", dur)
	}
}

// El techo absoluto corta aunque el stream no pare de moverse: un invitado (o
// un proveedor) que gotea para siempre no retiene una plaza para siempre.
func TestProxyTechoAbsoluto(t *testing.T) {
	srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}, credsUna(), conPlazos(time.Second, 600*time.Millisecond))

	t0 := time.Now()
	resp, err := pedirStream(t, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	dur := time.Since(t0)
	if err == nil {
		t.Fatal("el techo debería cortar el stream con error")
	}
	if dur < 600*time.Millisecond || dur > 3*time.Second {
		t.Errorf("cortado a los %v; quería ~600ms", dur)
	}
}

// Un invitado que anuncia un cuerpo y deja de mandarlo no retiene la petición
// más allá del plazo de inactividad, y no llega nada al proveedor.
func TestProxyNoEsperaAUnaSubidaParada(t *testing.T) {
	srv, hits := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {},
		credsUna(), conPlazos(300*time.Millisecond, 10*time.Second))

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /v1/x HTTP/1.1\r\nHost: example.com\r\nContent-Length: 100\r\n\r\nsolo diez.")
	t0 := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	dur := time.Since(t0)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("status %d con la subida a medias", resp.StatusCode)
		}
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("el proxy siguió esperando la subida pasado el plazo de inactividad")
	}
	if dur > 3*time.Second {
		t.Errorf("tardó %v en soltar la subida parada", dur)
	}
	if *hits != 0 {
		t.Errorf("el proveedor recibió %d peticiones", *hits)
	}
}

// El plazo de escritura que pone una petición no se queda en la conexión: la
// siguiente petición por la MISMA conexión, pasado ese plazo, funciona.
func TestProxyPlazoNoSeHeredaEnLaConexion(t *testing.T) {
	srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}, credsUna(), conPlazos(200*time.Millisecond, 400*time.Millisecond))

	cl := &http.Client{Transport: &http.Transport{}}
	pedir := func() (bool, string) {
		var reusada bool
		req, _ := http.NewRequest("GET", srv.URL+"/", nil)
		req.Host = "example.com"
		req = req.WithContext(httptrace.WithClientTrace(req.Context(),
			&httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reusada = i.Reused }}))
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return reusada, string(b)
	}
	if _, b := pedir(); b != "ok" {
		t.Fatalf("primera: %q", b)
	}
	time.Sleep(600 * time.Millisecond)
	reusada, b := pedir()
	if b != "ok" {
		t.Fatalf("segunda: %q", b)
	}
	if !reusada {
		t.Skip("el cliente no reutilizó la conexión; el test no prueba lo que quiere")
	}
}
