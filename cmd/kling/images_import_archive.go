package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
)

// IMPORTAR DE UN ARCHIVO: `kling image import -archive x.tar`.
//
// El archivo (docker save, o un layout OCI en tar o en un directorio) está en
// la máquina del CLI y el daemon puede estar en otra: el CLI lo abre y valida
// su estructura (internal/oci/archive.go), elige la imagen y sube al daemon,
// en flujo, los blobs que no tenga (PUT /oci/blobs/{digest}); el daemon
// comprueba cada uno con su sha256 y lo deja en su caché. Después pide la
// construcción oci con Source "archive" y el digest del manifiesto: el
// constructor no usa la red. La receta dice "archive" y el digest, nunca la
// ruta del archivo.

// archivoImport es lo que sale de abrir el archivo y elegir la imagen.
type archivoImport struct {
	a    *oci.Archive
	img  *oci.ArchiveImage
	arch string
}

// abrirArchivoImport abre el archivo y elige la imagen pick (o la única) para
// la arquitectura del daemon (o arch). Comprueba que el daemon sabe recibir
// blobs y el tope de tamaño (maxMB, 0 = el de siempre).
func abrirArchivoImport(ctx context.Context, c *api.Client, ruta, pick, arch string, maxMB int) (*archivoImport, error) {
	in, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	if !in.Has(api.CapabilityOCIBlobs) {
		return nil, fmt.Errorf("the daemon (%s) is too old to import an archive; update it", in.Version)
	}
	if arch == "" {
		arch = in.Arch
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	a, err := oci.OpenArchive(ruta)
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	img, err := a.Image(pick, arch)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("archive: %w", err)
	}
	if maxMB == 0 {
		maxMB = ociDefaultMaxMB
	}
	if img.LayerBytes > int64(maxMB)<<20 {
		a.Close()
		return nil, fmt.Errorf("the image's layers are %d MiB, over the %d MiB limit (-max-size)", img.LayerBytes>>20, maxMB)
	}
	return &archivoImport{a: a, img: img, arch: arch}, nil
}

// nombreArchivo es el nombre por defecto de una imagen de un archivo: como
// el de una referencia, o "archive-<id>" si no trae nombre.
func nombreArchivo(img *oci.ArchiveImage) string {
	if r, err := oci.ParseImageRef(img.Ref); img.Ref != "" && err == nil {
		return imageNameFor(r)
	}
	return "archive-" + strings.TrimPrefix(img.ConfigDigest, "sha256:")[:12]
}

// subir manda al daemon los blobs de la imagen que no tenga, uno detrás de
// otro y en flujo. log recibe una línea por blob (nil = nada).
func (ai *archivoImport) subir(ctx context.Context, c *api.Client, log io.Writer) (subidos int, bytes int64, err error) {
	for _, b := range ai.img.Blobs {
		cur, err := c.StatOCIBlob(ctx, b.Digest)
		if err != nil {
			return subidos, bytes, err
		}
		if cur != nil && cur.Size == b.Size {
			continue
		}
		if log != nil && b.Size >= 1<<20 {
			fmt.Fprintf(log, "  uploading %s %s (%d MiB)\n", b.What, shortDigest(b.Digest), b.Size>>20)
		}
		rc, err := ai.a.Open(b)
		if err != nil {
			return subidos, bytes, fmt.Errorf("%s: %w", b.What, err)
		}
		_, err = c.PutOCIBlob(ctx, b.Digest, rc, b.Size)
		rc.Close()
		if err != nil {
			return subidos, bytes, fmt.Errorf("uploading the %s (%s): %w", b.What, b.Digest, err)
		}
		subidos++
		bytes += b.Size
	}
	return subidos, bytes, nil
}

// archiveAlreadyLine adapta la línea de "ya importada" a una imagen de un
// archivo: no hay etiqueta que volver a resolver.
func archiveAlreadyLine(line string) string {
	line = strings.Replace(line, "-replace re-resolves the tag and rebuilds it", "-replace rebuilds it", 1)
	return strings.Replace(line, ", and re-resolves the tag)", ")", 1)
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}
