package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Peticion es lo que cada cliente repite.
type Peticion struct {
	URL       string
	Metodo    string
	Cuerpo    []byte
	Cabeceras map[string]string
}

// Punto es una tanda de carga con un número fijo de clientes.
type Punto struct {
	Clientes  int            `json:"clientes"`
	N         int            `json:"n"`
	Errores   int            `json:"errores"` // de red o con código fuera de 2xx
	Codigos   map[string]int `json:"codigos"` // "200", "503"… y "red" para los que no llegaron
	DuracionS float64        `json:"duracion_s"`
	RPS       float64        `json:"rps"`
	P50       float64        `json:"p50_ms"`
	P95       float64        `json:"p95_ms"`
	P99       float64        `json:"p99_ms"`
	Max       float64        `json:"max_ms"`
	// PrimerError es el texto del primer fallo de red, para saber qué pasó
	// sin repetir la tanda; vacío si no hubo.
	PrimerError string `json:"primer_error,omitempty"`
}

const maxClientes = 4096

// parseClientes lee "1,4,16" o "1 4 16".
func parseClientes(s string) ([]int, error) {
	var out []int
	for _, c := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(c)
		if err != nil || n < 1 || n > maxClientes {
			return nil, fmt.Errorf("clientes: %q no es un entero entre 1 y %d", c, maxClientes)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("clientes: lista vacía")
	}
	if strings.Contains(s, ",,") {
		return nil, fmt.Errorf("clientes: %q tiene un hueco", s)
	}
	return out, nil
}

// percentil es el de rango más cercano (sin interpolar: con pocas muestras
// da una que existió de verdad). No toca la entrada.
func percentil(ms []float64, p float64) float64 {
	if len(ms) == 0 {
		return 0
	}
	s := append([]float64(nil), ms...)
	sort.Float64s(s)
	if p <= 0 {
		return s[0]
	}
	i := int(float64(len(s))*p/100+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

// clienteHTTP habla por el socket Unix si se da uno (el host de la URL se
// ignora), o por TCP. Sin tope de conexiones por host: cada cliente de la carga
// es una conexión viva, como un cliente de verdad.
func clienteHTTP(socket string, timeout time.Duration) *http.Client {
	tr := &http.Transport{MaxIdleConns: maxClientes, MaxIdleConnsPerHost: maxClientes, IdleConnTimeout: 90 * time.Second}
	if socket != "" {
		var d net.Dialer
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", socket) }
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// cargar lanza clientes que repiten p sin pausa hasta sumar n peticiones (n >
// 0) o hasta que pase dur. Cada cliente espera su respuesta antes de mandar la
// siguiente: la concurrencia es exactamente clientes.
func cargar(ctx context.Context, c *http.Client, p Peticion, clientes int, dur time.Duration, n int) Punto {
	if dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dur)
		defer cancel()
	}
	var (
		mu      sync.Mutex
		lat     []float64
		codigos = map[string]int{}
		errores int
		primero string
		turnos  = make(chan struct{}, max(n, 0))
		wg      sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		turnos <- struct{}{}
	}
	close(turnos)
	uno := func() bool {
		if n > 0 {
			if _, ok := <-turnos; !ok {
				return false
			}
		}
		if ctx.Err() != nil {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, p.Metodo, p.URL, bytes.NewReader(p.Cuerpo))
		if err != nil {
			return false
		}
		if len(p.Cuerpo) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range p.Cabeceras {
			req.Header.Set(k, v)
		}
		t0 := time.Now()
		resp, err := c.Do(req)
		codigo := "red"
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codigo = strconv.Itoa(resp.StatusCode)
		} else if dur > 0 && ctx.Err() != nil {
			return false // la cortó el fin de la tanda: no cuenta
		}
		ms := float64(time.Since(t0).Microseconds()) / 1000
		mu.Lock()
		defer mu.Unlock()
		codigos[codigo]++
		if err != nil || resp.StatusCode/100 != 2 {
			errores++
			if err != nil && primero == "" {
				primero = err.Error()
			}
		}
		if err == nil {
			lat = append(lat, ms)
		}
		return true
	}
	t0 := time.Now()
	for i := 0; i < clientes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for uno() {
			}
		}()
	}
	wg.Wait()
	d := time.Since(t0).Seconds()
	total := 0
	for _, v := range codigos {
		total += v
	}
	pt := Punto{Clientes: clientes, N: total, Errores: errores, Codigos: codigos, DuracionS: d,
		P50: percentil(lat, 50), P95: percentil(lat, 95), P99: percentil(lat, 99), Max: percentil(lat, 100), PrimerError: primero}
	if d > 0 {
		pt.RPS = float64(total) / d
	}
	return pt
}
