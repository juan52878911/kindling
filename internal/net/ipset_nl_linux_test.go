//go:build linux

package net

import (
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
