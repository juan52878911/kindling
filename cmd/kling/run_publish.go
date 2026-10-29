package main

// `kling run -p HOST:GUEST`: un puerto estable en 127.0.0.1, como `docker run
// -p`. Lo publica el DAEMON (RunRequest.Publish, internal/publicar): resuelve
// en cada conexión el reenvío de la máquina (en el Mac cambia con cada
// arranque o descongelación) y la despierta si está congelada. kling se queda
// en primer plano siguiendo la salida del servicio; Ctrl-C deja de seguirla y
// la máquina y sus puertos se quedan (kling rm los quita). Nunca en otras
// interfaces.
//
// Contra un daemon sin api.CapabilityPublish, los publica este proceso
// mientras corre (publicarEnCLI), como antes.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// publishFlag acumula -p [127.0.0.1:]HOST:GUEST[/udp], -p PUERTO o
// -p :GUEST (uno libre al azar).
type publishFlag []api.PortPublish

func (p *publishFlag) String() string { return "" }

func (p *publishFlag) Set(v string) error {
	spec, proto := v, ""
	switch {
	case strings.HasSuffix(spec, "/udp"):
		spec, proto = strings.TrimSuffix(spec, "/udp"), api.ProtoUDP
	case strings.HasSuffix(spec, "/tcp"):
		spec = strings.TrimSuffix(spec, "/tcp")
	}
	partes := strings.Split(spec, ":")
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
		if i == 0 && s == "" {
			continue // :GUEST, uno libre al azar
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("-p %s: %q is not a port", v, s)
		}
		par[i] = n
	}
	*p = append(*p, api.PortPublish{Host: par[0], Guest: par[1], Proto: proto})
	return nil
}

// conPuertos añade los puertos TCP del invitado de ps a kling.ports (los que
// el host puede alcanzar) sin repetir.
func conPuertos(labels map[string]string, ps publishFlag) map[string]string {
	var tcp []int
	for _, p := range ps {
		if p.ProtoOr() == api.ProtoTCP {
			tcp = append(tcp, p.Guest)
		}
	}
	if len(tcp) == 0 {
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
	for _, p := range tcp {
		if !visto[p] {
			visto[p] = true
			l = append(l, p)
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

// publicarEnPrimerPlano enseña los puertos que publicó el daemon y sigue el
// servicio de mc hasta que se cancela ctx (Ctrl-C). Si el daemon no los
// publicó (uno anterior a api.CapabilityPublish), los publica este proceso
// mientras corre.
func publicarEnPrimerPlano(ctx context.Context, c *api.Client, host string, mc *api.Machine, ps publishFlag) {
	if len(mc.Published) > 0 {
		for _, p := range mc.Published {
			fmt.Fprintf(os.Stderr, "  %s\n", p.String())
		}
		fmt.Fprintf(os.Stderr, "Ctrl-C stops following; the machine and its ports stay (kling rm %s removes both)\n", mc.Name)
	} else {
		for _, p := range ps {
			if p.ProtoOr() != api.ProtoTCP {
				fmt.Fprintf(os.Stderr, "warning: -p %d/udp: this daemon does not publish ports; update it\n", p.Guest)
				continue
			}
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Host)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: cannot publish 127.0.0.1:%d (%v)\n", p.Host, err)
				continue
			}
			defer ln.Close()
			fmt.Fprintf(os.Stderr, "  127.0.0.1:%d -> %d\n", ln.Addr().(*net.TCPAddr).Port, p.Guest)
			go publish(ctx, c, ln, mc.ID, p.Guest)
		}
		fmt.Fprintf(os.Stderr, "Ctrl-C stops publishing; the machine stays (kling rm %s removes it)\n", mc.Name)
	}
	var mu sync.Mutex
	go followServiceLogs(ctx, host, mc.ID, "", false, &mu)
	<-ctx.Done()
}

// publicarEnCLI publica los ports: del compose desde este proceso (daemon
// sin api.CapabilityPublish): duran lo que dura `compose up`.
func publicarEnCLI(ctx context.Context, c *api.Client, t *traduccionCompose) []net.Listener {
	var lns []net.Listener
	for _, nodo := range clavesOrd(t.Publica) {
		for _, p := range t.Publica[nodo] {
			if p.ProtoOr() != api.ProtoTCP {
				fmt.Fprintf(os.Stderr, "warning: %s: %d/udp: this daemon does not publish ports; update it\n", nodo, p.Guest)
				continue
			}
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Host)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: cannot publish 127.0.0.1:%d (%v)\n", nodo, p.Host, err)
				continue
			}
			lns = append(lns, ln)
			fmt.Fprintf(os.Stderr, "  %-12s 127.0.0.1:%d -> %d\n", nodo, ln.Addr().(*net.TCPAddr).Port, p.Guest)
			go publish(ctx, c, ln, t.Proyect+"-"+nodo, p.Guest)
		}
	}
	return lns
}

// publish atiende 127.0.0.1:H y lleva cada conexión al puerto del invitado de
// la máquina, resolviendo su dirección en cada conexión (publicarEnCLI).
func publish(ctx context.Context, c *api.Client, ln net.Listener, machine string, port int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			up, err := dialMachine(ctx, c, machine, port)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s:%d: %v\n", machine, port, err)
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			cp := func(dst, src net.Conn) {
				_, _ = io.Copy(dst, src)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				done <- struct{}{}
			}
			go cp(up, conn)
			go cp(conn, up)
			<-done
			<-done
		}()
	}
}

// dialMachine conecta con el puerto del invitado: despierta la máquina si
// está congelada o en pausa, y reintenta unos segundos si el servicio aún no
// escucha (acaba de arrancar).
func dialMachine(ctx context.Context, c *api.Client, machine string, port int) (net.Conn, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		m, err := c.Get(ctx, machine)
		if err != nil {
			return nil, err
		}
		if m.State == api.StateWarm || m.State == api.StatePaused {
			if m, err = c.Thaw(ctx, machine); err != nil {
				return nil, err
			}
		}
		conn, err := net.DialTimeout("tcp", m.Addr(port), 2*time.Second)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// idleFreezeConfig lee daemon.idle_freeze (KLING_IDLE_FREEZE manda sobre el
// fichero). El daemon la consulta en cada vuelta de su barrido: se guarda un
// rato para no leer el fichero cada dos segundos.
func idleFreezeConfig() func() time.Duration {
	var mu sync.Mutex
	var leida time.Time
	var valor time.Duration
	return func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(leida) < 30*time.Second {
			return valor
		}
		v := loadConfig().Daemon.IdleFreeze
		if e, ok := os.LookupEnv("KLING_IDLE_FREEZE"); ok {
			v = e
		}
		d, err := config.ParseIdleFreeze(v, runtime.GOOS)
		if err != nil {
			d, _ = config.ParseIdleFreeze("", runtime.GOOS)
			if leida.IsZero() {
				fmt.Fprintf(os.Stderr, "warning: %v (using %s)\n", err, d)
			}
		}
		leida, valor = time.Now(), d
		return d
	}
}
