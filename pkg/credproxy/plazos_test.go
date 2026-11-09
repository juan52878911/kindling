package credproxy

import (
	"bufio"
	"context"
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
	}, credsUna(), conPlazos(500*time.Millisecond, 10*time.Second))

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
	// Márgenes anchos a propósito: con -race y la suite entera en paralelo el
	// planificador puede retrasar el vigía cientos de ms (un fallo en ~5 corridas
	// con 300 ms y tope de 3 s). El tope sigue muy por debajo del techo de 10 s:
	// si el plazo de inactividad no funcionara, esto seguiría fallando.
	if dur < 500*time.Millisecond || dur > 5*time.Second {
		t.Errorf("cortado a los %v; quería ~500ms (y en ningún caso el techo de 10 s)", dur)
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

// cierreLento retrasa el cierre del socket hacia el proveedor. tls.Conn.Close
// manda primero el close_notify y luego cierra el socket: en ese hueco el
// proveedor ve que el cliente se fue, su handler vuelve, net/http escribe el
// chunk final y el lector del transporte, aún vivo, lo lee. El cuerpo da
// io.EOF en vez del error del contexto. Sin el retraso el hueco es de
// microsegundos (TestProxyTechoAbsoluto fallaba así ~1 de cada 13 veces en CI).
type cierreLento struct{ net.Conn }

func (c cierreLento) Close() error {
	time.Sleep(100 * time.Millisecond)
	return c.Conn.Close()
}

// Cuando el proxy corta (techo o inactividad) y el proveedor contesta al corte
// cerrando su respuesta LIMPIA, el invitado tiene que ver un error igual: lo
// que llegó está truncado.
func TestProxyCorteConFinLimpioDelProveedor(t *testing.T) {
	casos := []struct {
		nombre    string
		idle, max time.Duration
		cada      time.Duration // 0: un evento y silencio
	}{
		{"techo", time.Second, 400 * time.Millisecond, 50 * time.Millisecond},
		{"inactividad", 300 * time.Millisecond, 10 * time.Second, 0},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
				for {
					if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					var tick <-chan time.Time
					if c.cada > 0 {
						tick = time.After(c.cada)
					}
					select {
					case <-r.Context().Done():
						return // fin limpio: net/http cierra el chunked
					case <-tick:
					}
				}
			}, credsUna(), func(p *Proxy) {
				conPlazos(c.idle, c.max)(p)
				tr := p.client.Transport.(*http.Transport)
				dial := tr.DialContext
				tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					cn, err := dial(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					return cierreLento{cn}, nil
				}
			})

			resp, err := pedirStream(t, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err == nil {
				t.Fatalf("el corte debería verse como error, llegó %q como si estuviera entero", body)
			}
			if !strings.Contains(string(body), "data: x") {
				t.Errorf("lo anterior al corte debería haber llegado: %q", body)
			}
		})
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
