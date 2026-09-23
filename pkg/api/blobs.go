package api

// Transferencia de imágenes entre daemons (GET/PUT /images/{name}/blob).
//
// Existe para macOS: allí no se construyen imágenes (hace falta root, loop y
// chroot de Linux), así que se construyen en un host Linux y se copian. El
// CLI hace de tubería entre los dos daemons y no guarda nada en disco.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Partes de una imagen que se pueden transferir.
const (
	BlobImage  = "image"  // el ext4 de una imagen monolítica
	BlobLayer  = "layer"  // la capa de una imagen por capas
	BlobRecipe = "recipe" // la receta (decide, entre otras cosas, la base de una capa)
	BlobKernel = "kernel" // el vmlinux compartido; va con el nombre KernelBlobName
)

// KernelBlobName es el nombre reservado con el que se transfiere el kernel.
const KernelBlobName = "vmlinux"

// Cabeceras de la transferencia.
const (
	HeaderSha256   = "X-Kling-Sha256" // sha256 en hexadecimal del contenido
	HeaderBlobPart = "X-Kling-Part"   // qué parte es (BlobImage, BlobLayer...)
)

// MaxBlobBytes es el tamaño máximo que acepta el daemon en un PUT de blob. Una
// imagen de herramientas con navegador ronda los 2-3 GiB; 16 GiB deja margen
// sin permitir que un cliente llene el disco del daemon con un solo envío.
const MaxBlobBytes int64 = 16 << 30

// BlobInfo describe un blob sin descargarlo.
type BlobInfo struct {
	Part   string
	Size   int64
	Sha256 string
}

// BlobPutResult es la respuesta de un PUT de blob.
type BlobPutResult struct {
	Name   string `json:"name"`
	Part   string `json:"part"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
	// Unchanged: ya había un fichero idéntico y no se tocó.
	Unchanged bool `json:"unchanged,omitempty"`
}

func blobURL(name, part string) string {
	u := "http://kling/images/" + url.PathEscape(name) + "/blob"
	if part != "" {
		u += "?" + url.Values{"part": {part}}.Encode()
	}
	return u
}

func blobInfoDe(resp *http.Response) *BlobInfo {
	return &BlobInfo{
		Part:   resp.Header.Get(HeaderBlobPart),
		Size:   resp.ContentLength,
		Sha256: resp.Header.Get(HeaderSha256),
	}
}

// StatImageBlob pregunta por un blob (HEAD). Un 404 vuelve como *StatusError.
func (c *Client) StatImageBlob(ctx context.Context, name, part string) (*BlobInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, blobURL(name, part), nil)
	if err != nil {
		return nil, err
	}
	// Por el cliente largo: el daemon calcula el sha256 antes de contestar, y
	// con una imagen de gigas eso pasa del minuto que se da a las cabeceras.
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, &StatusError{Code: resp.StatusCode, Message: resp.Status}
	}
	return blobInfoDe(resp), nil
}

// GetImageBlob abre un blob para leerlo. Quien llama cierra el lector.
func (c *Client) GetImageBlob(ctx context.Context, name, part string) (io.ReadCloser, *BlobInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL(name, part), nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, nil, statusErrorFrom(resp)
	}
	return resp.Body, blobInfoDe(resp), nil
}

// PutImageBlob envía un blob. size es obligatorio (el daemon rechaza lo que no
// cabe antes de leerlo) y sha256, si no es vacío, se verifica al terminar.
func (c *Client) PutImageBlob(ctx context.Context, name, part string, r io.Reader, size int64, sha256 string) (*BlobPutResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, blobURL(name, part), r)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	if sha256 != "" {
		req.Header.Set(HeaderSha256, sha256)
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, statusErrorFrom(resp)
	}
	b, err := LeerCuerpo(resp.Body, 1<<16)
	if err != nil {
		return nil, err
	}
	var out BlobPutResult
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SizeString es el tamaño legible de un blob, para los mensajes del CLI.
func (b *BlobInfo) SizeString() string {
	if b == nil || b.Size < 0 {
		return "?"
	}
	const mib = 1 << 20
	if b.Size >= mib {
		return strconv.FormatInt(b.Size/mib, 10) + " MiB"
	}
	return strconv.FormatInt(b.Size, 10) + " B"
}

// CopyStep es un paso de CopyImage, para enseñar progreso.
type CopyStep struct {
	Name string // imagen (o KernelBlobName)
	Part string
	Size int64
	// Skipped: el destino ya tenía un fichero idéntico y no se mandó nada.
	Skipped bool
}

type blobRef struct{ name, part string }

// planCopia decide qué partes hay que mover para que `name` arranque en el
// destino: el kernel, la cadena de bases de una imagen por capas (primero la
// base, que la capa no sirve sin ella) y, de cada imagen, su receta ANTES que
// su capa, porque es la receta la que dice sobre qué base va la capa.
func planCopia(imgs []Image, name string) ([]blobRef, error) {
	porNombre := make(map[string]Image, len(imgs))
	for _, i := range imgs {
		porNombre[i.Name] = i
	}
	plan := []blobRef{{KernelBlobName, BlobKernel}}
	visto := map[string]bool{}
	var visitar func(n string, via string) error
	visitar = func(n, via string) error {
		if visto[n] {
			return nil
		}
		visto[n] = true
		img, ok := porNombre[n]
		if !ok {
			if via != "" {
				return fmt.Errorf("image %q is layered on %q, which is not on the source daemon", via, n)
			}
			return fmt.Errorf("image %q does not exist on the source daemon", n)
		}
		if img.Base != "" {
			if err := visitar(img.Base, n); err != nil {
				return err
			}
		}
		if img.HasRecipe {
			plan = append(plan, blobRef{n, BlobRecipe})
		}
		if img.Base != "" {
			plan = append(plan, blobRef{n, BlobLayer})
		} else {
			plan = append(plan, blobRef{n, BlobImage})
		}
		return nil
	}
	if err := visitar(name, ""); err != nil {
		return nil, err
	}
	return plan, nil
}

// CopyImage copia una imagen de un daemon a otro, en flujo: lo que lee de src
// lo escribe en dst sin tocar el disco local. Incluye el kernel y, si la
// imagen va por capas, su base. Lo que el destino ya tiene idéntico no se
// vuelve a mandar.
func CopyImage(ctx context.Context, src, dst *Client, name string, progress func(CopyStep)) error {
	si, err := src.Info(ctx)
	if err != nil {
		return fmt.Errorf("source daemon: %w", err)
	}
	di, err := dst.Info(ctx)
	if err != nil {
		return fmt.Errorf("destination daemon: %w", err)
	}
	for _, x := range []struct {
		quien string
		info  *Info
	}{{"source", si}, {"destination", di}} {
		if !x.info.Has("image-blobs") {
			return fmt.Errorf("the %s daemon (%s) is too old to copy images: it needs version 0.9 or later", x.quien, x.info.Version)
		}
	}
	// Una imagen x86_64 no arranca en un Mac, ni al revés: mejor decirlo antes
	// de mover gigas.
	if si.Arch != "" && di.Arch != "" && si.Arch != di.Arch {
		return fmt.Errorf("the source daemon is %s and the destination %s: images and kernels are built per "+
			"architecture. Copy from a %s host (on a Mac: a Linux arm64 VM)", si.Arch, di.Arch, di.Arch)
	}
	imgs, err := src.Images(ctx)
	if err != nil {
		return fmt.Errorf("listing images on the source: %w", err)
	}
	plan, err := planCopia(imgs, name)
	if err != nil {
		return err
	}
	for _, b := range plan {
		paso, err := copiarBlob(ctx, src, dst, b)
		if err != nil {
			return fmt.Errorf("copying the %s of %q: %w", b.part, b.name, err)
		}
		if progress != nil {
			progress(paso)
		}
	}
	return nil
}

func copiarBlob(ctx context.Context, src, dst *Client, b blobRef) (CopyStep, error) {
	paso := CopyStep{Name: b.name, Part: b.part}
	part := b.part
	if b.name == KernelBlobName {
		part = ""
	}
	body, info, err := src.GetImageBlob(ctx, b.name, part)
	if err != nil {
		return paso, err
	}
	defer body.Close()
	paso.Size = info.Size
	if info.Sha256 == "" || info.Size < 0 {
		return paso, fmt.Errorf("the source did not send the size and sha256")
	}
	// Si el destino ya lo tiene idéntico no se manda: cortar la descarga cuesta
	// menos que subir otra vez gigas que no cambian nada.
	if cur, err := dst.StatImageBlob(ctx, b.name, part); err == nil && cur.Sha256 == info.Sha256 {
		paso.Skipped = true
		return paso, nil
	}
	if _, err := dst.PutImageBlob(ctx, b.name, part, body, info.Size, info.Sha256); err != nil {
		return paso, err
	}
	return paso, nil
}
