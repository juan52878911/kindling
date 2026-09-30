package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una imagen es monolítica o por capas: subir la otra forma encima no se
// acepta (la capa no se usaría nunca, o quedaría mezclada con el ext4), y no
// deja nada escrito.
func TestPutImageBlobNoMezclaCapaYMonolitica(t *testing.T) {
	s, root := servidorBlobs(t)
	imgs := filepath.Join(root, "images")

	if rr := putBlob(s, "mono", api.BlobImage, "ext4 entero", ""); rr.Code != http.StatusCreated {
		t.Fatalf("PUT monolítica = %d %s", rr.Code, rr.Body)
	}
	if rr := putBlob(s, "mono", api.BlobLayer, "capa", ""); rr.Code != http.StatusConflict {
		t.Fatalf("capa sobre monolítica = %d %s, quería 409", rr.Code, rr.Body)
	}
	if _, err := os.Stat(filepath.Join(imgs, "mono.layer.ext4")); !os.IsNotExist(err) {
		t.Errorf("quedó una capa junto a la monolítica: %v", err)
	}

	if rr := putBlob(s, "capas", api.BlobLayer, "capa", ""); rr.Code != http.StatusCreated {
		t.Fatalf("PUT capa = %d %s", rr.Code, rr.Body)
	}
	if rr := putBlob(s, "capas", api.BlobImage, "ext4 entero", ""); rr.Code != http.StatusConflict {
		t.Fatalf("monolítica sobre capa = %d %s, quería 409", rr.Code, rr.Body)
	}
	if _, err := os.Stat(filepath.Join(imgs, "capas.ext4")); !os.IsNotExist(err) {
		t.Errorf("quedó un ext4 junto a la capa: %v", err)
	}
	sinTemporales(t, imgs)

	// La misma forma sigue pudiendo sustituirse.
	if rr := putBlob(s, "mono", api.BlobImage, "ext4 nuevo", ""); rr.Code != http.StatusCreated {
		t.Fatalf("sustituir la monolítica = %d %s", rr.Code, rr.Body)
	}
}
