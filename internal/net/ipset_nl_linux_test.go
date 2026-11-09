//go:build linux

package net

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Contra el kernel de verdad: el mensaje netlink mete la IP con su timeout en
// el set de ESE netns, y si el netns se borra y se crea otro con el mismo
// nombre, la siguiente IP va al nuevo (no al socket del viejo). Pide root e
// ipset: en el lab, dentro de `unshare --net --mount` con /run/netns en tmpfs.
func TestAnadirIPSetNetlinkKernel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("pide root")
	}
	if _, err := exec.LookPath("ipset"); err != nil {
		t.Skip("sin ipset")
	}
	ns := "kl-nltest" + strconv.Itoa(os.Getpid()%1000)
	set := setNameFromNS(ns)
	sh := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
		return string(out)
	}
	montar := func() {
		sh("ip", "netns", "add", ns)
		sh("ip", "netns", "exec", ns, "ipset", "create", set, "hash:ip", "timeout", "0")
	}
	montar()
	t.Cleanup(func() {
		cerrarIPSet(ns)
		_ = exec.Command("ip", "netns", "del", ns).Run()
	})

	if err := anadirIPSetNetlink(ns, set, "93.184.216.34", 300); err != nil {
		t.Fatal(err)
	}
	if err := anadirIPSetNetlink(ns, set, "93.184.216.35", 0); err != nil {
		t.Fatal(err)
	}
	// -exist: repetirla renueva, no falla.
	if err := anadirIPSetNetlink(ns, set, "93.184.216.34", 600); err != nil {
		t.Fatalf("repetida: %v", err)
	}
	l := sh("ip", "netns", "exec", ns, "ipset", "list", set)
	if !strings.Contains(l, "93.184.216.34 timeout") || !strings.Contains(l, "93.184.216.35 timeout 0") {
		t.Fatalf("ipset list:\n%s", l)
	}
	if err := anadirIPSetNetlink(ns, "no-existe", "1.2.3.4", 0); err == nil {
		t.Fatal("un set que no existe no dio error")
	}

	sh("ip", "netns", "del", ns)
	montar()
	if err := anadirIPSetNetlink(ns, set, "93.184.216.36", 0); err != nil {
		t.Fatal(err)
	}
	if l := sh("ip", "netns", "exec", ns, "ipset", "list", set); !strings.Contains(l, "93.184.216.36") {
		t.Fatalf("tras recrear el netns la IP no fue al nuevo:\n%s", l)
	}
}

// Con el set lleno, el kernel contesta HASH_FULL por netlink: anadirIPSet
// devuelve ese rechazo y no lanza el comando (que daría el mismo error con un
// fork por consulta). Pide root e ipset, como el de arriba.
func TestSetLlenoNoCaeAlComandoKernel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("pide root")
	}
	if _, err := exec.LookPath("ipset"); err != nil {
		t.Skip("sin ipset")
	}
	ns := "kl-nlfull" + strconv.Itoa(os.Getpid()%1000)
	set := setNameFromNS(ns)
	for _, args := range [][]string{
		{"ip", "netns", "add", ns},
		{"ip", "netns", "exec", ns, "ipset", "create", set, "hash:ip", "timeout", "0", "maxelem", "2"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	t.Cleanup(func() {
		cerrarIPSet(ns)
		_ = exec.Command("ip", "netns", "del", ns).Run()
	})
	oldE := ejecutar
	t.Cleanup(func() { ejecutar = oldE })
	procesos := 0
	ejecutar = func(args ...string) error { procesos++; return oldE(args...) }

	for _, ip := range []string{"93.184.216.1", "93.184.216.2"} {
		if err := anadirIPSet(ns, set, ip, 60); err != nil {
			t.Fatal(err)
		}
	}
	err := anadirIPSet(ns, set, "93.184.216.3", 60)
	var rechazo *rechazoIPSet
	if !errors.As(err, &rechazo) {
		t.Fatalf("con el set lleno: err = %v, quería el rechazo del kernel", err)
	}
	if procesos != 0 {
		t.Fatalf("lanzó %d procesos de ipset", procesos)
	}
	// La conexión sigue sirviendo: el rechazo no la cierra.
	if err := anadirIPSet(ns, set, "93.184.216.1", 120); err != nil || procesos != 0 {
		t.Fatalf("tras el rechazo: err %v, %d procesos", err, procesos)
	}
}
