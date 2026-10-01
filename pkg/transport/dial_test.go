package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewSinEndpointUsaElSocketLocal(t *testing.T) {
	if d := New(""); d.Endpoint != DefaultSocketPath() {
		t.Errorf("New(\"\") = %q, esperaba %q", d.Endpoint, DefaultSocketPath())
	}
	if d := New("ssh://juan@lab"); d.Endpoint != "ssh://juan@lab" {
		t.Errorf("New no respeto el endpoint dado: %q", d.Endpoint)
	}
}

func TestDialLlegaAlSocketConYSinEsquema(t *testing.T) {
	dir, err := os.MkdirTemp("", "tr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aqui: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// Las dos formas documentadas tienen que llegar al mismo sitio.
	for _, endpoint := range []string{sock, "unix://" + sock} {
		c, err := New(endpoint).Dial(context.Background())
		if err != nil {
			t.Errorf("Dial(%q): %v", endpoint, err)
			continue
		}
		c.Close()
	}
}

// El daemon de microVMs equivale a root en su host: puede montar discos y
// arrancar kernels arbitrarios. Por eso NUNCA escucha en un puerto de red, y por
// eso el dialer no debe aprender a hablar TCP.
//
// Este test existe para que anadir un `case "tcp://"` se ponga rojo. Es
// exactamente el error que costo a Docker una decada de servidores comprometidos,
// y el comentario de cabecera del paquete ya lo advierte — pero un comentario no
// falla cuando alguien lo ignora.
func TestElDialerNoAbreConexionesDeRed(t *testing.T) {
	// Un servidor TCP local de verdad: si el dialer aprendiera a hablar TCP, se
	// conectaria aqui y el test lo veria.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sin TCP local: %v", err)
	}
	defer ln.Close()
	conectado := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			conectado <- struct{}{}
			c.Close()
		}
	}()

	for _, endpoint := range []string{
		"tcp://" + ln.Addr().String(),
		"http://" + ln.Addr().String(),
		ln.Addr().String(), // "127.0.0.1:54321" a secas
	} {
		c, err := New(endpoint).Dial(context.Background())
		if err == nil {
			c.Close()
			t.Errorf("Dial(%q) abrio una conexion; el daemon no debe ser alcanzable por red", endpoint)
			continue
		}
		// Y el error tiene que hablar del socket, no de la red: es lo que le dice
		// a quien se equivoco que aqui no hay un puerto que buscar.
		if !strings.Contains(err.Error(), "cannot talk to the daemon") {
			t.Errorf("Dial(%q) fallo con un error que despista: %v", endpoint, err)
		}
	}

	select {
	case <-conectado:
		t.Error("alguien se conecto al puerto TCP: el dialer habla red")
	default:
	}
}

// conBases apunta el caché del usuario y /tmp a directorios de la prueba, para
// no depender (ni ensuciar) el HOME real de quien corre los tests.
func conBases(t *testing.T, cache, tmp string) {
	t.Helper()
	oldCache, oldTmp := userCacheDir, tmpBase
	t.Cleanup(func() { userCacheDir, tmpBase = oldCache, oldTmp })
	userCacheDir = func() (string, error) {
		if cache == "" {
			return "", errors.New("sin directorio de cache")
		}
		return cache, nil
	}
	tmpBase = tmp
}

// dirCorto es un directorio temporal de ruta corta: t.TempDir() en macOS cuelga
// de /var/folders/... y ya roza el límite de sun_path por sí solo.
func dirCorto(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "trc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// El multiplexado ahorra el apreton de manos SSH en las llamadas siguientes
// (`kling try` hace varias por invocacion). El directorio del socket de
// control tiene que ser 0700: si fuera compartido, otro usuario del host
// podria apuntar su propio ssh al mismo ControlPath y colarse en la conexion
// ya autenticada.
func TestSSHMultiplexArgsIncluyeControlMasterConDirectorioPropio(t *testing.T) {
	cache := dirCorto(t)
	conBases(t, cache, dirCorto(t))

	args := sshMultiplexArgs("juan@lab")
	joined := strings.Join(args, " ")
	for _, want := range []string{"ControlMaster=auto", "ControlPersist=60s", "ControlPath=" + cache} {
		if !strings.Contains(joined, want) {
			t.Fatalf("faltan los argumentos de multiplexado (%q): %v", want, args)
		}
	}
	info, err := os.Stat(filepath.Join(cache, "kindling", "ssh-control"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("permisos del directorio de control = %o, esperaba 0700", perm)
	}
}

// ssh escucha primero en "<ControlPath>.<16 caracteres>": si esa ruta no cabe
// en sun_path, ssh sale con 255 y la llamada entera falla. Así se rompió kling
// contra ssh:// en un Mac con el ControlPath en ~/Library/Caches.
func TestSSHControlPathCabeEnSunPathConElSufijoDeSSH(t *testing.T) {
	conBases(t, dirCorto(t), dirCorto(t))
	path := sshControlPath("juan@lab")
	if path == "" {
		t.Fatal("con directorios cortos debería multiplexar")
	}
	if len(path)+sshTempSuffix >= maxSunPath {
		t.Errorf("%q + sufijo de ssh = %d bytes, el límite es %d", path, len(path)+sshTempSuffix, maxSunPath)
	}
	if otro := sshControlPath("juan@otro"); otro == path {
		t.Error("dos destinos distintos comparten socket de control")
	}
}

// Un caché con una ruta tan larga que el socket no cabe cae a /tmp/kling-<uid>.
func TestSSHControlPathLargoCaeATmp(t *testing.T) {
	tmp := dirCorto(t)
	conBases(t, "/"+strings.Repeat("x", maxSunPath), tmp)
	path := sshControlPath("juan@lab")
	if !strings.HasPrefix(path, tmp+"/kling-") {
		t.Fatalf("sshControlPath = %q, esperaba que cayera a %s", path, tmp)
	}
}

// Un directorio de control que otros pueden escribir no vale: se prueba el
// siguiente, y si no queda ninguno, sin multiplexado.
func TestSSHControlPathRechazaDirectorioCompartido(t *testing.T) {
	cache, tmp := dirCorto(t), dirCorto(t)
	conBases(t, cache, tmp)
	for _, d := range []string{
		filepath.Join(cache, "kindling", "ssh-control"),
		filepath.Join(tmp, fmt.Sprintf("kling-%d", os.Getuid())),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if path := sshControlPath("juan@lab"); path != "" {
		t.Errorf("sshControlPath = %q con los dos directorios compartidos, esperaba \"\"", path)
	}
}

// Si no se puede preparar el directorio del socket de control (sin HOME, FS de
// solo lectura...), dialSSH tiene que seguir funcionando: cae al modo de
// siempre en vez de fallar la conexion entera por no poder multiplexarla.
func TestSSHMultiplexArgsSinDirectorioCaeAlModoDeSiempre(t *testing.T) {
	conBases(t, "", "/nonexistent/ro")

	args := sshMultiplexArgs("juan@lab")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "ControlMaster") {
		t.Errorf("deberia caer al modo sin multiplexado: %v", args)
	}
	if !strings.Contains(joined, "BatchMode=yes") || !strings.Contains(joined, "ConnectTimeout=10") {
		t.Errorf("perdio los argumentos base al no poder multiplexar: %v", args)
	}
}

// Un socket con una ruta que no cabe en sun_path se alcanza por un enlace
// corto, tanto desde el cliente como desde dial-stdio; antes daba un
// "connect: invalid argument" sin explicación.
func TestDialRutaLargaPorEnlaceCorto(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "trl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	oldTmp := tmpBase
	t.Cleanup(func() { tmpBase = oldTmp })
	tmpBase = base

	hondo := filepath.Join(base, strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(hondo, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(hondo, "kling.sock")
	if len(sock) < maxSunPath {
		t.Fatalf("la ruta de prueba mide %d bytes, debería pasar de %d", len(sock), maxSunPath)
	}
	// Escuchar con una ruta relativa corta: lo mismo que hace kling-vz.
	t.Chdir(hondo)
	ln, err := net.Listen("unix", "kling.sock")
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aqui: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("ok"))
			c.Close()
		}
	}()

	for i := 0; i < 2; i++ { // la segunda reutiliza el enlace
		c, err := New("unix://" + sock).Dial(context.Background())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		b := make([]byte, 2)
		if _, err := c.Read(b); err != nil || string(b) != "ok" {
			t.Fatalf("leido %q, %v", b, err)
		}
		c.Close()
	}
	var out strings.Builder
	if err := ServeStdio(sock, strings.NewReader(""), &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	// Si el directorio de enlaces no es privado, el error lo explica.
	if err := os.Chmod(filepath.Join(base, fmt.Sprintf("kling-%d", os.Getuid())), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = New(sock).Dial(context.Background())
	if err == nil || !strings.Contains(err.Error(), "a unix socket allows") {
		t.Fatalf("esperaba un error que explicara el tope, no %v", err)
	}
}
