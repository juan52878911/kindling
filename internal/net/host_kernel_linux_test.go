//go:build linux

package net

import (
	"fmt"
	stdnet "net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"
)

// Estos tests montan reglas de iptables de verdad, así que solo corren en un
// netns de red propio y desechable: root, KLING_TEST_NETNS_AISLADO=1 y un
// netns distinto del de PID 1. En el lab:
//
//	sudo env KLING_TEST_NETNS_AISLADO=1 unshare --net --mount sh -c \
//	  'mount -t tmpfs none /run/netns && ./net.test -test.run Kernel'
//
// Así nada toca el cortafuegos del propio host.
func requiereNetnsAislado(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("KLING_TEST_NETNS_AISLADO") != "1" {
		t.Skip("pide root y KLING_TEST_NETNS_AISLADO=1 dentro de unshare --net --mount")
	}
	propio, err1 := os.Readlink("/proc/self/ns/net")
	init, err2 := os.Readlink("/proc/1/ns/net")
	if err1 != nil || err2 != nil || propio == init {
		t.Skip("no estoy en un netns aislado: no toco el cortafuegos del host")
	}
}

func shT(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
}

// enNetns corre f con el hilo dentro del netns ns. Un socket creado ahí se
// queda en ese netns aunque el hilo vuelva.
func enNetns(t *testing.T, ns string, f func()) {
	t.Helper()
	runtime.LockOSThread()
	propio, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	defer propio.Close()
	destino, err := os.Open("/var/run/netns/" + ns)
	if err != nil {
		t.Fatal(err)
	}
	defer destino.Close()
	if err := setns(int(destino.Fd())); err != nil {
		t.Fatal(err)
	}
	f()
	if err := setns(int(propio.Fd())); err != nil {
		// El hilo se queda bloqueado y muere con la goroutine.
		t.Fatalf("no vuelvo al netns del test: %v", err)
	}
	runtime.UnlockOSThread()
}

func escuchar(t *testing.T, ns, dir string) {
	t.Helper()
	var l stdnet.Listener
	enNetns(t, ns, func() {
		var err error
		if l, err = stdnet.Listen("tcp", dir); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
}

func conecta(t *testing.T, ns, dir string) bool {
	t.Helper()
	ok := false
	marcar := func() {
		c, err := stdnet.DialTimeout("tcp", dir, 700*time.Millisecond)
		if err == nil {
			c.Close()
			ok = true
		}
	}
	if ns == "" {
		marcar()
	} else {
		enNetns(t, ns, marcar)
	}
	return ok
}

// S1: un contenedor con red puente de Docker no llega al agente (8080) de una
// máquina, aunque Docker ponga sus ACCEPT de FORWARD delante de todo (lo que
// hace cada vez que arranca). Los demás puertos de la máquina siguen como
// antes, y el daemon (root, desde el host) sí llega.
func TestKernelAgenteCerradoAUnContenedorDocker(t *testing.T) {
	requiereNetnsAislado(t)
	shT(t, "ip", "link", "set", "lo", "up")
	// La "máquina": un netns con su veth, como lo deja Setup.
	shT(t, "ip", "netns", "add", "kl-s1maq")
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", "kl-s1maq").Run() })
	shT(t, "ip", "link", "add", "vh-s1maq", "type", "veth", "peer", "name", "vg-s1maq", "netns", "kl-s1maq")
	shT(t, "ip", "addr", "add", "172.30.9.1/30", "dev", "vh-s1maq")
	shT(t, "ip", "link", "set", "vh-s1maq", "up")
	shT(t, "ip", "-n", "kl-s1maq", "addr", "add", "172.30.9.2/30", "dev", "vg-s1maq")
	shT(t, "ip", "-n", "kl-s1maq", "link", "set", "vg-s1maq", "up")
	shT(t, "ip", "-n", "kl-s1maq", "route", "add", "default", "via", "172.30.9.1")
	// El contenedor, en el puente docker0.
	shT(t, "ip", "link", "add", "docker0", "type", "bridge")
	shT(t, "ip", "addr", "add", "172.17.0.1/16", "dev", "docker0")
	shT(t, "ip", "link", "set", "docker0", "up")
	shT(t, "ip", "netns", "add", "s1ct")
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", "s1ct").Run() })
	shT(t, "ip", "link", "add", "dk-s1", "type", "veth", "peer", "name", "eth0", "netns", "s1ct")
	shT(t, "ip", "link", "set", "dk-s1", "master", "docker0", "up")
	shT(t, "ip", "-n", "s1ct", "addr", "add", "172.17.0.2/16", "dev", "eth0")
	shT(t, "ip", "-n", "s1ct", "link", "set", "eth0", "up")
	shT(t, "ip", "-n", "s1ct", "route", "add", "default", "via", "172.17.0.1")

	escuchar(t, "kl-s1maq", "172.30.9.2:8080")
	escuchar(t, "kl-s1maq", "172.30.9.2:5432")

	if err := SetupHost(AccesoAgente{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inputMontado.Store(false); accesoMontado.Store(nil) })
	// Docker después de kindling: sus cadenas, saltadas desde lo alto de
	// FORWARD, y la política DROP.
	for _, r := range [][]string{
		{"-N", "DOCKER-USER"},
		{"-N", "DOCKER-FORWARD"},
		{"-A", "DOCKER-USER", "-j", "RETURN"},
		{"-A", "DOCKER-FORWARD", "-i", "docker0", "!", "-o", "docker0", "-j", "ACCEPT"},
		{"-A", "DOCKER-FORWARD", "-o", "docker0", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"-I", "FORWARD", "1", "-j", "DOCKER-FORWARD"},
		{"-I", "FORWARD", "1", "-j", "DOCKER-USER"},
		{"-P", "FORWARD", "DROP"},
	} {
		shT(t, append([]string{"iptables"}, r...)...)
	}

	if !conecta(t, "s1ct", "172.30.9.2:5432") {
		t.Fatal("el contenedor no llega a un puerto de servicio de la máquina: la topología del test no funciona")
	}
	if conecta(t, "s1ct", "172.30.9.2:8080") {
		t.Error("un contenedor con red puente llega al agente de la máquina")
	}
	if !conecta(t, "", "172.30.9.2:8080") {
		t.Error("el daemon (root, en el host) no llega al agente")
	}
}

// P1: lo que tarda Setup + Teardown en un host de verdad. No es un test sino
// una medición: KLING_MEDIR_RED=N vueltas.
func TestKernelMedirSetupRed(t *testing.T) {
	requiereNetnsAislado(t)
	vueltas, _ := strconv.Atoi(os.Getenv("KLING_MEDIR_RED"))
	if vueltas <= 0 {
		t.Skip("KLING_MEDIR_RED=N para medir")
	}
	shT(t, "ip", "link", "set", "lo", "up")
	if err := SetupHost(AccesoAgente{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inputMontado.Store(false); accesoMontado.Store(nil) })
	for _, e := range []Egress{EgressNone, EgressInternet} {
		var ms []float64
		for i := 0; i < vueltas; i++ {
			n := Plan(i, fmt.Sprintf("med%05d", i))
			t0 := time.Now()
			if err := n.Setup(e, nil, 0); err != nil {
				t.Fatalf("Setup %s: %v", e, err)
			}
			ms = append(ms, float64(time.Since(t0).Microseconds())/1000)
			n.Teardown()
		}
		slices.Sort(ms)
		t.Logf("Setup %-8s n=%d mediana %.1f ms  p90 %.1f ms  min %.1f ms", e, vueltas, ms[len(ms)/2], ms[len(ms)*9/10], ms[0])
	}
}
