package guest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// fakeIP deja en un PATH propio un `ip` de mentira que apunta sus argumentos y
// sale con el código pedido escribiendo msg en stderr.
func fakeIP(t *testing.T, code int, msg string) (argsFile string) {
	t.Helper()
	// Que netlink no llegue al kernel: en un Linux como root tocaría la
	// tabla de rutas de quien corre la prueba. Y estas prueban el respaldo.
	sinNetlink(t, errors.ErrUnsupported)
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\necho '" + msg + "' >&2\nexit " +
		strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argsFile
}

func capturarLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// La ruta se pone a través del cosechador y un `ip` que va bien se reporta
// como bien (antes, siendo PID 1, el cosechador se llevaba el estado y salía
// "no child processes").
func TestSetupMMDSRoutePoneLaRuta(t *testing.T) {
	args := fakeIP(t, 0, "")
	buf := capturarLog(t)
	SetupMMDSRoute()

	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatalf("no se ejecutó ip: %v", err)
	}
	if strings.TrimSpace(string(got)) != "route add 169.254.169.254/32 dev eth0" {
		t.Errorf("argumentos = %q", got)
	}
	if !strings.Contains(buf.String(), "route to 169.254.169.254 via eth0 ready") {
		t.Errorf("log = %q, want la ruta lista", buf.String())
	}
}

// "File exists" no es un fallo: la ruta ya estaba.
func TestSetupMMDSRouteYaExistente(t *testing.T) {
	fakeIP(t, 2, "RTNETLINK answers: File exists")
	buf := capturarLog(t)
	SetupMMDSRoute()
	if buf.Len() != 0 {
		t.Errorf("registró algo con la ruta ya puesta: %q", buf.String())
	}
}

// Cualquier otro fallo se registra con la salida de ip.
func TestSetupMMDSRouteFalloSeRegistra(t *testing.T) {
	fakeIP(t, 1, "Cannot find device eth0")
	buf := capturarLog(t)
	SetupMMDSRoute()
	if !strings.Contains(buf.String(), "could not set the route") || !strings.Contains(buf.String(), "Cannot find device") {
		t.Errorf("log = %q", buf.String())
	}
}

// sinNetlink sustituye la llamada de netlink por una que da err (nil: la ruta
// se puso) y apunta con qué se la llamó.
func sinNetlink(t *testing.T, err error) *[]string {
	t.Helper()
	var llamadas []string
	antes := rutaNetlink
	rutaNetlink = func(dst [4]byte, dev string) error {
		llamadas = append(llamadas, fmt.Sprintf("%d.%d.%d.%d %s", dst[0], dst[1], dst[2], dst[3], dev))
		return err
	}
	t.Cleanup(func() { rutaNetlink = antes })
	return &llamadas
}

// Por netlink, sin `ip`: ni se busca en el PATH.
func TestSetupMMDSRoutePorNetlink(t *testing.T) {
	llamadas := sinNetlink(t, nil)
	t.Setenv("PATH", t.TempDir()) // sin ip
	buf := capturarLog(t)
	SetupMMDSRoute()
	if len(*llamadas) != 1 || (*llamadas)[0] != "169.254.169.254 eth0" {
		t.Fatalf("netlink calls = %q", *llamadas)
	}
	if !strings.Contains(buf.String(), "route to 169.254.169.254 via eth0 ready") {
		t.Errorf("log = %q, want la ruta lista", buf.String())
	}
}

// EEXIST por netlink es "ya estaba", como el "File exists" de ip: sin ruido
// y sin caer a ip.
func TestSetupMMDSRouteNetlinkYaExistente(t *testing.T) {
	sinNetlink(t, syscall.EEXIST)
	t.Setenv("PATH", t.TempDir())
	buf := capturarLog(t)
	SetupMMDSRoute()
	if buf.Len() != 0 {
		t.Errorf("registró algo con la ruta ya puesta: %q", buf.String())
	}
}

// Si netlink falla por otra cosa, se prueba con ip.
func TestSetupMMDSRouteNetlinkFallaCaeAIP(t *testing.T) {
	args := fakeIP(t, 0, "")
	sinNetlink(t, syscall.EPERM)
	capturarLog(t)
	SetupMMDSRoute()
	if _, err := os.ReadFile(args); err != nil {
		t.Fatalf("no se ejecutó ip: %v", err)
	}
}

// El mensaje es el de `ip route add 169.254.169.254/32 dev <if>`, y la
// respuesta se lee del NLMSG_ERROR con el mismo número de secuencia.
func TestMensajeRuta(t *testing.T) {
	le := binary.NativeEndian
	b := mensajeRuta(7, [4]byte{169, 254, 169, 254}, 3)
	if len(b) != 44 || le.Uint32(b[0:]) != 44 {
		t.Fatalf("len = %d / %d, want 44", len(b), le.Uint32(b[0:]))
	}
	if le.Uint16(b[4:]) != rtmNewRoute || le.Uint16(b[6:]) != 0x605 || le.Uint32(b[8:]) != 7 {
		t.Fatalf("header = % x", b[:16])
	}
	if got := b[16:28]; got[0] != 2 || got[1] != 32 || got[4] != 254 || got[6] != 253 || got[7] != 1 {
		t.Fatalf("rtmsg = % x", got)
	}
	if le.Uint16(b[28:]) != 8 || le.Uint16(b[30:]) != rtaDst || !bytes.Equal(b[32:36], []byte{169, 254, 169, 254}) {
		t.Fatalf("RTA_DST = % x", b[28:36])
	}
	if le.Uint16(b[36:]) != 8 || le.Uint16(b[38:]) != rtaOif || le.Uint32(b[40:]) != 3 {
		t.Fatalf("RTA_OIF = % x", b[36:44])
	}
	ack := func(seq uint32, errno int32) []byte {
		r := make([]byte, 36)
		le.PutUint32(r[0:], 36)
		le.PutUint16(r[4:], nlmsgErrorType)
		le.PutUint32(r[8:], seq)
		le.PutUint32(r[16:], uint32(-errno))
		return r
	}
	if e, ok := errnoRespuesta(ack(7, 0), 7); !ok || e != 0 {
		t.Fatalf("ACK: errno=%d ok=%v", e, ok)
	}
	if e, ok := errnoRespuesta(append(ack(6, 0), ack(7, 17)...), 7); !ok || e != 17 {
		t.Fatalf("EEXIST after another answer: errno=%d ok=%v", e, ok)
	}
	if _, ok := errnoRespuesta(ack(6, 0), 7); ok {
		t.Fatal("took another sequence's answer")
	}
	if _, ok := errnoRespuesta([]byte{1, 2, 3}, 7); ok {
		t.Fatal("took a truncated answer")
	}
}
