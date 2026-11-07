package daemon

import (
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// putQueSeCalla abre un PUT que promete 1 MiB, manda unos bytes y se calla
// sin cerrar la conexión: el cliente que deja la subida colgada.
func putQueSeCalla(t *testing.T, s *Server, ruta string) net.Conn {
	t.Helper()
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "PUT %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", ruta, 1<<20)
	if _, err := c.Write([]byte("unos pocos bytes")); err != nil {
		t.Fatal(err)
	}
	return c
}

// conPlazoCorto acorta el plazo de inactividad de las subidas para el test.
func conPlazoCorto(t *testing.T) {
	t.Helper()
	viejo := plazoSubidaInactiva
	plazoSubidaInactiva = 200 * time.Millisecond
	t.Cleanup(func() { plazoSubidaInactiva = viejo })
}

func esperar(t *testing.T, que string, cond func() bool) {
	t.Helper()
	for fin := time.Now().Add(5 * time.Second); time.Now().Before(fin); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("a los 5 s sigue sin pasar: %s", que)
}

func temporales(dir, sufijo string) []string {
	var out []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if strings.Contains(e.Name(), sufijo) {
			out = append(out, e.Name())
		}
	}
	return out
}

// Una subida de imagen que se calla se corta por inactividad y no deja el
// temporal (ni el FD) retenido.
func TestPutImageBlobCortaSiElClienteSeCalla(t *testing.T) {
	conPlazoCorto(t)
	s, root := servidorBlobs(t)
	putQueSeCalla(t, s, "/images/colgada/blob?part=image")
	dir := filepath.Join(root, "images")
	esperar(t, "aparece el temporal", func() bool { return len(temporales(dir, ".tmp-")) > 0 })
	esperar(t, "se borra el temporal", func() bool { return len(temporales(dir, ".tmp-")) == 0 })
}

// Igual en la caché OCI, que además suelta el cerrojo: los barridos vuelven
// a poder correr.
func TestPutOCIBlobCortaSiElClienteSeCalla(t *testing.T) {
	conPlazoCorto(t)
	s, root := servidorBlobs(t)
	d := "sha256:" + strings.Repeat("ab", 32)
	putQueSeCalla(t, s, "/oci/blobs/"+d)
	dir := filepath.Join(root, "cache", "oci", "sha256")
	esperar(t, "aparece el .part", func() bool { return len(temporales(dir, ".part")) > 0 })
	esperar(t, "se suelta el cerrojo de la caché", func() bool {
		if s.muCacheOCI.TryLock() {
			s.muCacheOCI.Unlock()
			return true
		}
		return false
	})
	esperar(t, "se borra el .part", func() bool { return len(temporales(dir, ".part")) == 0 })
}
