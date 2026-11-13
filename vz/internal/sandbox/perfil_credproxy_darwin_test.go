//go:build darwin

package sandbox

// El proxy de credenciales aparte (kling-credproxy) frente a los perfiles: el
// VMM conecta a su socket de reenvío pero no escribe nada en su directorio (ni
// un socket falso donde el daemon entrega las claves, ni el registro, ni lo
// borra), ni lee sus derramados; y el perfil del proxy le deja su directorio,
// el broker y nada más del disco.

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/server"
)

// raizCorta es una raíz de kindling bajo /tmp (sun_path) con la máquina m y
// el directorio del proxy, ya resueltas.
func raizCorta(t *testing.T) (root, mdir, cpdir string) {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "sbc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	root = Real(d)
	mdir = filepath.Join(root, "machines", "m")
	cpdir = filepath.Join(mdir, credproxy.SplitDir)
	for _, p := range []string{filepath.Join(cpdir, credproxy.SplitTempDir)} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(cpdir, credproxy.AuditFile), filepath.Join(cpdir, credproxy.SplitTempDir, "cuerpo"),
		filepath.Join(mdir, "overlay.ext4")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, mdir, cpdir
}

func escucharUnix(t *testing.T, p string) {
	t.Helper()
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
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

func TestPerfilVMMFrenteAlProxyAparte(t *testing.T) {
	for nombre, perfil := range perfiles(t) {
		t.Run(nombre, func(t *testing.T) {
			root, mdir, cpdir := raizCorta(t)
			fwd, api := filepath.Join(cpdir, credproxy.SplitFwdSock), filepath.Join(cpdir, credproxy.SplitAPISock)
			escucharUnix(t, fwd)
			escucharUnix(t, api)
			pares, err := Parametros(root, mdir, "", server.Confinamiento{ConRed: true})
			if err != nil {
				t.Fatal(err)
			}
			pruebas := []struct {
				p    prueba
				want string
			}{
				{prueba{"unix", fwd}, "ok"},
				{prueba{"unix", api}, "denied"},
				{prueba{"read", filepath.Join(cpdir, credproxy.AuditFile)}, "ok"},
				{prueba{"write", filepath.Join(cpdir, credproxy.AuditFile)}, "denied"},
				{prueba{"write", filepath.Join(cpdir, credproxy.AuditFile+".1")}, "denied"},
				{prueba{"read", filepath.Join(cpdir, credproxy.SplitTempDir, "cuerpo")}, "denied"},
				{prueba{"remove", api}, "denied"},
				{prueba{"remove", cpdir}, "denied"},
				{prueba{"bindunix", filepath.Join(cpdir, "falso.sock")}, "denied"},
				{prueba{"mkdir", filepath.Join(cpdir, "otro")}, "denied"},
				// Lo suyo sigue igual: su registro y sus sockets en MDIR.
				{prueba{"write", filepath.Join(mdir, credproxy.AuditFileVMM)}, "ok"},
				{prueba{"write", filepath.Join(mdir, credproxy.AuditFile)}, "denied"},
				{prueba{"bindunix", filepath.Join(mdir, "x.sock")}, "ok"},
			}
			o := orden{Perfil: perfil, Pares: pares}
			for _, p := range pruebas {
				o.Pruebas = append(o.Pruebas, p.p)
			}
			r := encerrado(t, o)
			for i, p := range pruebas {
				if r.Res[i] != p.want {
					t.Errorf("%s %s: %s, want %s", p.p.Op, p.p.Addr, r.Res[i], p.want)
				}
			}
			if _, err := os.Stat(api); err != nil {
				t.Errorf("the API socket is gone: %v", err)
			}
		})
	}
}

func TestPerfilCredproxy(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "kling-credproxy", "kling-credproxy.sb"))
	if err != nil {
		t.Fatal(err)
	}
	root, mdir, cpdir := raizCorta(t)
	broker, otro := filepath.Join(root, "broker.sock"), filepath.Join(root, "otro.sock")
	escucharUnix(t, broker)
	escucharUnix(t, otro)
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "k"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fuera := escucha(t, 40000, 40999) // un servicio en el loopback del Mac
	pruebas := []struct {
		p    prueba
		want string
	}{
		{prueba{"write", filepath.Join(cpdir, credproxy.AuditFile)}, "ok"},
		{prueba{"write", filepath.Join(cpdir, credproxy.AuditFile+".1")}, "ok"},
		{prueba{"read", filepath.Join(cpdir, credproxy.SplitTempDir, "cuerpo")}, "ok"},
		{prueba{"bindunix", filepath.Join(cpdir, "y.sock")}, "ok"},
		{prueba{"unix", broker}, "ok"},
		{prueba{"unix", otro}, "denied"},
		{prueba{"read", filepath.Join(mdir, "overlay.ext4")}, "denied"},
		{prueba{"write", filepath.Join(mdir, "fuera")}, "denied"},
		{prueba{"write", filepath.Join(root, "fuera")}, "denied"},
		{prueba{"read", filepath.Join(root, "secrets", "k")}, "denied"},
		{prueba{"connect", fuera}, "denied"},
		{prueba{"bind", "127.0.0.1:0"}, "denied"},
	}
	o := orden{Perfil: string(b), Pares: []string{"DIR", cpdir, "BROKER", broker}}
	for _, p := range pruebas {
		o.Pruebas = append(o.Pruebas, p.p)
	}
	r := encerrado(t, o)
	for i, p := range pruebas {
		if r.Res[i] != p.want {
			t.Errorf("%s %s: %s, want %s", p.p.Op, p.p.Addr, r.Res[i], p.want)
		}
	}
	t.Logf("sandbox_init of kling-credproxy.sb: %.1f ms", float64(r.InitNs)/1e6)
}
