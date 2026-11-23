// ia-bench es el cliente de carga y el servidor de mentira del banco de IA
// (scripts/96-von-bench.sh, scripts/bench/remoto/ia.sh). Saca JSON por la
// salida estándar.
//
//	ia-bench carga  -url http://gw/v1/m/chispa/v1/decide [-socket gw.sock] [-body '{…}'|@f.json]
//	                [-clientes 1,4,16] [-duracion 20s | -n 200] [-token-file f]
//	ia-bench guest  -machine M -port 8000 -path /v1/decide [-body …] [-wait 60s]
//	                una petición a una réplica por el daemon (POST /machines/M/guest), cronometrada
//	ia-bench falso  -listen :8000 -mem-mib 512 [-tocar] [-retardo 0]
//	                servidor HTTP de mentira con memoria del tamaño pedido
//
// No es parte de kling: es una herramienta del banco y no entra en ningún
// binario publicado.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "uso: ia-bench carga|guest|falso [opciones] (ia-bench <orden> -h)")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "carga":
		err = ordenCarga(ctx, os.Args[2:])
	case "guest":
		err = ordenGuest(ctx, os.Args[2:])
	case "falso":
		err = ordenFalso(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("orden desconocida: %s", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ia-bench:", err)
		os.Exit(1)
	}
}

// cuerpo lee "@fichero" o devuelve el texto tal cual.
func cuerpo(s string) ([]byte, error) {
	if f, ok := strings.CutPrefix(s, "@"); ok {
		return os.ReadFile(f)
	}
	return []byte(s), nil
}

func ordenCarga(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("carga", flag.ContinueOnError)
	url := fs.String("url", "", "URL de la petición (con -socket, el host da igual)")
	socket := fs.String("socket", "", "socket Unix del gateway en vez de TCP")
	metodo := fs.String("method", "POST", "método")
	body := fs.String("body", "", "cuerpo, o @fichero")
	clientes := fs.String("clientes", "1,4,16", "clientes simultáneos de cada tanda")
	dur := fs.Duration("duracion", 20*time.Second, "duración de cada tanda (si no se da -n)")
	n := fs.Int("n", 0, "peticiones por tanda en vez de una duración")
	calentar := fs.Int("calentar", 1, "peticiones de calentamiento antes de cada tanda, fuera de la cuenta")
	timeout := fs.Duration("timeout", 2*time.Minute, "tope de cada petición")
	tokenFile := fs.String("token-file", "", "fichero con el token (Authorization: Bearer); nunca se imprime")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("falta -url")
	}
	cs, err := parseClientes(*clientes)
	if err != nil {
		return err
	}
	b, err := cuerpo(*body)
	if err != nil {
		return err
	}
	p := Peticion{URL: *url, Metodo: *metodo, Cuerpo: b, Cabeceras: map[string]string{}}
	if *tokenFile != "" {
		t, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		p.Cabeceras["Authorization"] = "Bearer " + strings.TrimSpace(string(t))
	}
	c := clienteHTTP(*socket, *timeout)
	var puntos []Punto
	for _, k := range cs {
		if *calentar > 0 {
			cargar(ctx, c, p, 1, 0, *calentar)
		}
		d := *dur
		if *n > 0 {
			d = 0
		}
		puntos = append(puntos, cargar(ctx, c, p, k, d, *n))
		if ctx.Err() != nil {
			break
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"url": *url, "metodo": *metodo, "puntos": puntos})
}

func ordenGuest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("guest", flag.ContinueOnError)
	host := fs.String("host", "", "daemon (por defecto el del CLI: KLING_HOST, contexto o socket local)")
	machine := fs.String("machine", "", "máquina")
	port := fs.Int("port", 8000, "puerto en el invitado")
	path := fs.String("path", "/health", "ruta en el invitado")
	metodo := fs.String("method", "POST", "método")
	body := fs.String("body", "", "cuerpo, o @fichero")
	wait := fs.Duration("wait", 0, "reintentar hasta un 2xx durante este tiempo (arranque en frío)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *machine == "" {
		return errors.New("falta -machine")
	}
	b, err := cuerpo(*body)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	c := api.NewClient(cfg.Host(*host))
	t0 := time.Now()
	intentos := 0
	for {
		intentos++
		t := time.Now()
		resp, err := c.Guest(ctx, *machine, api.GuestRequest{Port: *port, Path: *path, Method: *metodo, Body: string(b)})
		ms := float64(time.Since(t).Microseconds()) / 1000
		ok := err == nil && resp.Status/100 == 2
		if ok || *wait <= 0 || time.Since(t0) > *wait || ctx.Err() != nil {
			out := map[string]any{"ms": ms, "total_ms": float64(time.Since(t0).Microseconds()) / 1000, "intentos": intentos}
			if err != nil {
				out["error"] = err.Error()
			} else {
				out["status"], out["bytes"] = resp.Status, len(resp.Body)
			}
			if e := json.NewEncoder(os.Stdout).Encode(out); e != nil {
				return e
			}
			if !ok {
				return errors.New("la réplica no contestó 2xx")
			}
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func ordenFalso(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("falso", flag.ContinueOnError)
	listen := fs.String("listen", ":8000", "dirección TCP")
	memMiB := fs.Int("mem-mib", 256, "MiB de memoria «del modelo»")
	tocar := fs.Bool("tocar", true, "cada petición lee todas las páginas (como una inferencia)")
	retardo := fs.Duration("retardo", 0, "espera añadida a cada petición")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *memMiB < 0 || *memMiB > 1<<20 {
		return fmt.Errorf("-mem-mib fuera de rango: %d", *memMiB)
	}
	t0 := time.Now()
	f := nuevoFalso(*memMiB, *tocar, *retardo)
	fmt.Fprintf(os.Stderr, "ia-bench falso: %d MiB listos en %d ms; escucho en %s\n", *memMiB, time.Since(t0).Milliseconds(), *listen)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
