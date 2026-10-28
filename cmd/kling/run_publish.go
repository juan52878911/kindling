package main

// `kling run -p HOST:GUEST`: un puerto estable en 127.0.0.1, como `docker run
// -p`. En el Mac cada máquina se alcanza por un puerto de loopback que cambia
// con cada arranque o descongelación (Machine.Forwards); con -p, kling se
// queda en primer plano publicando 127.0.0.1:HOST con el mismo publicador que
// `kling compose up` (resuelve el reenvío en cada conexión y despierta la
// máquina si está congelada) y siguiendo la salida del servicio. Ctrl-C deja
// de publicar; la máquina se queda. Nunca en otras interfaces.

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/juan52878911/kindling/pkg/api"
)

// publishFlag acumula -p [127.0.0.1:]HOST:GUEST o -p PUERTO.
type publishFlag [][2]int

func (p *publishFlag) String() string { return "" }

func (p *publishFlag) Set(v string) error {
	partes := strings.Split(v, ":")
	if len(partes) == 3 {
		if partes[0] != "127.0.0.1" && partes[0] != "localhost" {
			return fmt.Errorf("-p %s: only 127.0.0.1 (kling never publishes on other interfaces)", v)
		}
		partes = partes[1:]
	}
	if len(partes) == 1 {
		partes = append(partes, partes[0])
	}
	if len(partes) != 2 {
		return fmt.Errorf("-p %s: use HOST:GUEST or PORT", v)
	}
	var par [2]int
	for i, s := range partes {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "/tcp"))
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("-p %s: %q is not a TCP port", v, s)
		}
		par[i] = n
	}
	*p = append(*p, par)
	return nil
}

// conPuertos añade los puertos del invitado de ps a kling.ports (los que el
// host puede alcanzar) sin repetir.
func conPuertos(labels map[string]string, ps [][2]int) map[string]string {
	if len(ps) == 0 {
		return labels
	}
	out := map[string]string{}
	for k, v := range labels {
		out[k] = v
	}
	visto := map[int]bool{}
	var l []int
	for _, s := range strings.Split(out[api.LabelPorts], ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && !visto[n] {
			visto[n] = true
			l = append(l, n)
		}
	}
	for _, p := range ps {
		if !visto[p[1]] {
			visto[p[1]] = true
			l = append(l, p[1])
		}
	}
	sort.Ints(l)
	s := make([]string, len(l))
	for i, n := range l {
		s[i] = strconv.Itoa(n)
	}
	out[api.LabelPorts] = strings.Join(s, ",")
	return out
}

// escucharPuertos abre 127.0.0.1:HOST de cada par antes de arrancar nada: un
// puerto ocupado se dice sin dejar una máquina a medias.
func escucharPuertos(ps [][2]int) ([]net.Listener, error) {
	var lns []net.Listener
	for _, p := range ps {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p[0])))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return nil, fmt.Errorf("-p %d:%d: %w", p[0], p[1], err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// publicarEnPrimerPlano publica los puertos de mc y sigue su servicio hasta
// que se cancela ctx (Ctrl-C).
func publicarEnPrimerPlano(ctx context.Context, c *api.Client, host string, mc *api.Machine, ps [][2]int, lns []net.Listener) {
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	for i, p := range ps {
		fmt.Fprintf(os.Stderr, "  127.0.0.1:%d -> %d\n", p[0], p[1])
		go publish(ctx, c, lns[i], mc.ID, p[1])
	}
	fmt.Fprintf(os.Stderr, "Ctrl-C stops publishing; the machine stays (kling rm %s removes it)\n", mc.Name)
	var mu sync.Mutex
	go followServiceLogs(ctx, host, mc.ID, "", &mu)
	<-ctx.Done()
}
