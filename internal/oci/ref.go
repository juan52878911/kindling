package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// ETIQUETAS. Pull solo acepta digests: una etiqueta se mueve y lo que se
// construyó tiene que poder decirse exactamente. Resolve hace la única
// traducción permitida, etiqueta → digest, una vez y al principio; quien la
// llama apunta el digest en la receta y desde ahí ya no hay etiqueta.

// ImageRef es una referencia de imagen como la escribe Docker:
// [registro/]repo[:etiqueta][@sha256:...].
type ImageRef struct {
	Registry, Repo string
	Tag            string // "" si viene solo con digest
	Digest         string // "" si viene solo con etiqueta
}

var (
	// Como la gramática de distribution/reference, sin mayúsculas en el repo.
	reRepo = lazyre.New(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	reTag  = lazyre.New(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	reHost = lazyre.New(`^[A-Za-z0-9.-]+(?::[0-9]+)?$`)
)

// String es la forma completa: registro/repo[:etiqueta][@digest].
func (r ImageRef) String() string {
	s := r.Registry + "/" + r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Name es registro/repo, lo que esperan Pull y ParseRef.
func (r ImageRef) Name() string { return r.Registry + "/" + r.Repo }

// ParseImageRef parte una referencia. Sin etiqueta ni digest es ":latest",
// como en Docker.
func ParseImageRef(s string) (ImageRef, error) {
	var r ImageRef
	if strings.ContainsAny(s, " \t\r\n\x00") || s == "" || len(s) > 512 {
		return r, fmt.Errorf("invalid image reference %q", s)
	}
	name := s
	if i := strings.Index(name, "@"); i >= 0 {
		name, r.Digest = name[:i], name[i+1:]
		if !reDigest.MatchString(r.Digest) {
			return r, fmt.Errorf("invalid digest in %q (want sha256:<64 hex>)", s)
		}
	}
	// La etiqueta es lo que va tras el último ":" si no hay "/" detrás (el
	// ":" de "localhost:5000/x" es del puerto).
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i:], "/") {
		name, r.Tag = name[:i], name[i+1:]
		if !reTag.MatchString(r.Tag) {
			return r, fmt.Errorf("invalid tag in %q", s)
		}
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	r.Registry, r.Repo = ParseRef(name)
	if !reHost.MatchString(r.Registry) || !reRepo.MatchString(r.Repo) {
		return r, fmt.Errorf("invalid image reference %q", s)
	}
	return r, nil
}

// Resolve da el digest de la etiqueta de ref (o el que ya traiga). El
// digest sale del sha256 del manifiesto que se bajó, no de la cabecera del
// registro, y si la cabecera dice otro es un error. El manifiesto queda en
// la caché: Pull no lo vuelve a pedir.
func (c *Client) Resolve(ctx context.Context, ref ImageRef) (string, error) {
	if ref.Digest != "" {
		return ref.Digest, nil
	}
	if !reTag.MatchString(ref.Tag) {
		return "", fmt.Errorf("invalid tag %q", ref.Tag)
	}
	resp, err := c.do(ctx, ref.Registry, ref.Repo, "manifests/"+ref.Tag, manifestAccept)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := readMax(resp.Body, maxManifest)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	// Antes que el digest: el de un schema1 firmado no es el sha256 de lo
	// que llega, y el error sería otro.
	if schema1(body, resp.Header.Get("Content-Type")) {
		return "", errSchema1(ref.String())
	}
	digest := sha(body)
	if h := resp.Header.Get("Docker-Content-Digest"); h != "" && h != digest {
		return "", fmt.Errorf("%s: registry says %s but the manifest it sent is %s", ref, h, digest)
	}
	dst := c.BlobPath(digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
		if os.WriteFile(dst+".part", body, 0o644) == nil {
			_ = os.Rename(dst+".part", dst)
		}
	}
	c.logf("%s is %s", ref, digest)
	return digest, nil
}
