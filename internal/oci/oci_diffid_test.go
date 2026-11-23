package oci_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

// imagenConDiffID publica una imagen de una capa (gzip o zstd) cuya config
// declara diffID como el de esa capa.
func imagenConDiffID(r *ocitest.Registry, capa []byte, diffID string) string {
	cfg, _ := json.Marshal(map[string]any{"architecture": "arm64", "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{diffID}}})
	cd := r.Put(cfg, "")
	mt := "application/vnd.oci.image.layer.v1.tar+gzip"
	if strings.HasPrefix(string(capa), "\x28\xb5\x2f\xfd") {
		mt = "application/vnd.oci.image.layer.v1.tar+zstd"
	}
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)},
		"layers": []map[string]any{{"mediaType": mt, "digest": r.Put(capa, ""), "size": len(capa)}}})
	return r.Put(m, "application/vnd.oci.image.manifest.v1+json")
}

// importar hace lo que el constructor: Pull y, por cada capa, el árbol con
// AddTar. Devuelve la imagen (si el Pull pasó) y el primer error.
func importar(t *testing.T, r *ocitest.Registry, man string, unpack bool) (*oci.Image, error) {
	t.Helper()
	c := &oci.Client{Cache: t.TempDir(), MaxBytes: 1 << 30}
	if unpack {
		c.Unpack = t.TempDir()
	}
	img, err := c.Pull(context.Background(), r.Host()+"/mal/img", man, "arm64")
	if err != nil {
		return nil, err
	}
	root := ext4.NewDir(0o755, 0, 0, time.Unix(1, 0))
	for i, l := range img.Layers {
		rc, err := oci.OpenLayer(l)
		if err != nil {
			return img, err
		}
		err = root.AddTar(rc, ext4.TarOptions{Stream: i, Whiteouts: true, Time: time.Unix(1, 0)})
		rc.Close()
		if err != nil {
			return img, err
		}
	}
	return img, nil
}

// Una imagen cuya config declara un diff_id que no es el sha256 de su capa
// descomprimida no se importa (con Unpack falla el Pull; sin él, la lectura
// de la capa), en gzip y en zstd: si pasara, su cadena de diff_ids haría de
// base (BestBase) de una imagen ajena. Con el diff_id de verdad, sí entra.
func TestDiffIDFalsoSeRechaza(t *testing.T) {
	files := []ocitest.File{{Name: "lib/libc.so.6", Body: "TROYANO"}}
	tarb := ocitest.Tar(files)
	h := sha256.Sum256(tarb)
	bueno := "sha256:" + hex.EncodeToString(h[:])
	falso := "sha256:" + strings.Repeat("ab", 32) // el diff_id de la base de otra imagen
	capas := map[string][]byte{"gzip": ocitest.TarGz(files), "zstd": ocitest.Zstd(tarb)}
	for nombre, capa := range capas {
		for _, unpack := range []bool{true, false} {
			t.Run(nombre+map[bool]string{true: "/unpack", false: "/sin-unpack"}[unpack], func(t *testing.T) {
				r := ocitest.New()
				defer r.Close()
				img, err := importar(t, r, imagenConDiffID(r, capa, falso), unpack)
				if err == nil {
					chain := img.Config.RootFS.DiffIDs
					bases := []imagen.BaseOCI{{Name: "mal", Arch: "arm64", DiffIDs: chain, Init: "sh", Agent: "a"}}
					b := imagen.BestBase(bases, []string{falso, "sha256:" + strings.Repeat("cd", 32)}, "a", "arm64")
					t.Fatalf("aceptada con diff_ids=%v (BestBase la elegiría de base: %v)", chain, b != nil)
				}
				if !strings.Contains(err.Error(), "diff_id") {
					t.Errorf("el error no habla del diff_id: %v", err)
				}
				if unpack && img != nil {
					t.Errorf("con Unpack tenía que fallar el Pull, falló después: %v", err)
				}
				if _, err := importar(t, r, imagenConDiffID(r, capa, bueno), unpack); err != nil {
					t.Fatalf("con el diff_id de verdad: %v", err)
				}
			})
		}
	}
}

// Quien lee la capa hasta el final con OpenLayer (sin AddTar) recibe el
// error del diff_id en vez de io.EOF.
func TestDiffIDFalsoAlLeerHastaEOF(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	capa := ocitest.TarGz([]ocitest.File{{Name: "a", Body: "x"}})
	man := imagenConDiffID(r, capa, "sha256:"+strings.Repeat("00", 32))
	c := &oci.Client{Cache: t.TempDir()}
	img, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := oci.OpenLayer(img.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 64<<10)
	for err == nil {
		_, err = rc.Read(buf)
	}
	if !strings.Contains(err.Error(), "diff_id") {
		t.Fatalf("al llegar al final: %v", err)
	}
}
