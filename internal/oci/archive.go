package oci

// ARCHIVOS LOCALES: lo que deja `docker save` (un tar con manifest.json y
// repositories) y el layout OCI (index.json, oci-layout y blobs/sha256/, en un
// tar o en un directorio). Se leen en la máquina del CLI, que puede no ser la
// del daemon: aquí solo se valida la estructura y se dice qué blobs hacen
// falta y dónde están; quien los sube (cmd/kling, PUT /oci/blobs/{digest})
// los manda en flujo y el daemon comprueba cada uno por su sha256 antes de
// dejarlo en la caché. El constructor oci construye luego desde la caché sin
// red (Client.Offline), comprobando la cadena manifiesto → configuración →
// capas (y los diff_ids) como con un registro.
//
// docker save no da un manifiesto OCI: su manifest.json dice qué fichero es la
// configuración y cuáles las capas, que van SIN comprimir (layer.tar) y cuyo
// sha256 es el diff_id de la configuración. Con eso se escribe un manifiesto
// OCI equivalente (capas application/vnd.oci.image.layer.v1.tar, digest =
// diff_id), que es el que se sube y el que apunta la receta.
//
// Lo que no se acepta de un archivo: nombres absolutos o con "..", enlaces
// (simbólicos o duros) que salgan del archivo, entradas repetidas, ficheros
// dispersos y tars comprimidos. Un enlace DENTRO del archivo se resuelve por
// nombre en el propio índice (docker save enlaza un layer.tar repetido al
// primero), nunca en el disco. En un directorio, nada que no sea un fichero
// regular: ni un enlace.

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// Formatos de archivo.
const (
	FormatDockerArchive = "docker-archive"
	FormatOCILayout     = "oci-layout"
)

// Tipos de lo que se escribe para un docker save.
const (
	MediaOCIConfig   = "application/vnd.oci.image.config.v1+json"
	MediaOCILayerTar = "application/vnd.oci.image.layer.v1.tar"
)

const (
	// maxArchiveEntries es el tope de entradas de un tar (docker save pone
	// tres o cuatro por capa).
	maxArchiveEntries = 100_000
	// maxArchiveImages es el tope de imágenes de un archivo.
	maxArchiveImages = 1000
	// maxLinkHops es cuántos enlaces internos se siguen hasta un fichero.
	maxLinkHops = 8
)

// Anotaciones con el nombre de una imagen en un layout OCI.
const (
	annotRefName       = "org.opencontainers.image.ref.name"
	annotContainerName = "io.containerd.image.name"
)

// archiveFS es de dónde se leen los ficheros de un archivo.
type archiveFS interface {
	// size da el tamaño de un fichero regular (resueltos los enlaces internos).
	size(name string) (int64, error)
	open(name string) (io.ReadCloser, error)
	close() error
}

// Archive es un archivo abierto.
type Archive struct {
	Format string
	fs     archiveFS
	docker []dockerSaveEntry
	index  []indexEntry
}

// dockerSaveEntry es una imagen del manifest.json de docker save.
type dockerSaveEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// indexEntry es una entrada del index.json de un layout OCI, con sus nombres.
type indexEntry struct {
	Descriptor
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ArchiveImage es una imagen elegida de un archivo: lo que hay que subir.
type ArchiveImage struct {
	Format string
	// Ref es su nombre en el archivo ("postgres:17-alpine"), si tiene.
	Ref            string
	ManifestDigest string
	ConfigDigest   string
	// Blobs son el manifiesto, la configuración y las capas (sin repetir).
	Blobs []ArchiveBlob
	// LayerBytes es lo que suman las capas, como las declara el manifiesto.
	LayerBytes int64
}

// ArchiveBlob es un blob de una imagen de un archivo.
type ArchiveBlob struct {
	Digest string
	Size   int64
	// What es qué es, para los mensajes: "manifest", "config", "layer 2/5".
	What string
	name string // en el archivo
	data []byte // o el contenido, si se escribió aquí (el manifiesto de docker save)
}

// OpenArchive abre un tar (docker save o layout OCI) o un directorio con un
// layout OCI.
func OpenArchive(p string) (*Archive, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	var fsys archiveFS
	if st.IsDir() {
		r, err := os.OpenRoot(p)
		if err != nil {
			return nil, err
		}
		fsys = &dirFS{r}
	} else {
		if fsys, err = openTarFS(p); err != nil {
			return nil, err
		}
	}
	a := &Archive{fs: fsys}
	if err := a.load(); err != nil {
		fsys.close()
		return nil, err
	}
	return a, nil
}

// Close cierra el archivo.
func (a *Archive) Close() error { return a.fs.close() }

func (a *Archive) load() error {
	_, errIdx := a.fs.size("index.json")
	_, errLay := a.fs.size("oci-layout")
	switch {
	case errIdx == nil && errLay == nil:
		a.Format = FormatOCILayout
		var lay struct {
			Version string `json:"imageLayoutVersion"`
		}
		if err := a.readJSON("oci-layout", 4096, &lay); err != nil {
			return err
		}
		if lay.Version != "1.0.0" {
			return fmt.Errorf("oci-layout: unsupported imageLayoutVersion %q", lay.Version)
		}
		var idx struct {
			Manifests []indexEntry `json:"manifests"`
		}
		if err := a.readJSON("index.json", maxManifest, &idx); err != nil {
			return err
		}
		if len(idx.Manifests) > maxArchiveImages {
			return fmt.Errorf("index.json lists %d images (max %d)", len(idx.Manifests), maxArchiveImages)
		}
		for _, e := range idx.Manifests {
			if !attestation(e) {
				a.index = append(a.index, e)
			}
		}
		if len(a.index) == 0 {
			return errors.New("index.json lists no images")
		}
		return nil
	default:
		if _, err := a.fs.size("manifest.json"); err != nil {
			return errors.New("not a docker save archive (manifest.json) nor an OCI layout (index.json and oci-layout)")
		}
		a.Format = FormatDockerArchive
		if err := a.readJSON("manifest.json", maxManifest, &a.docker); err != nil {
			return err
		}
		if len(a.docker) == 0 || len(a.docker) > maxArchiveImages {
			return fmt.Errorf("manifest.json lists %d images (want 1..%d)", len(a.docker), maxArchiveImages)
		}
		return nil
	}
}

// readJSON lee un fichero pequeño del archivo.
func (a *Archive) readJSON(name string, max int64, v any) error {
	b, err := a.read(name, max)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (a *Archive) read(name string, max int64) ([]byte, error) {
	n, err := a.fs.size(name)
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("%s is %d bytes, over the %d limit", name, n, max)
	}
	rc, err := a.fs.open(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readMax(rc, max)
}

// Names son los nombres de las imágenes del archivo, ordenados.
func (a *Archive) Names() []string {
	var out []string
	for _, e := range a.docker {
		out = append(out, e.RepoTags...)
	}
	for _, e := range a.index {
		if n := entryName(e); n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// attestation dice si una entrada de index.json es una atestación (la
// procedencia o el SBOM que docker save, desde la 25, guarda junto a la
// imagen) y no una imagen.
func attestation(e indexEntry) bool {
	return e.Annotations["io.containerd.manifest.subject"] != "" ||
		e.Annotations["vnd.docker.reference.type"] == "attestation-manifest" ||
		(e.Platform != nil && e.Platform.OS == "unknown")
}

// entryName es el nombre de una entrada de index.json: el completo que pone
// docker/containerd o, si no, el de la anotación OCI (a veces solo la
// etiqueta).
func entryName(e indexEntry) string {
	if n := e.Annotations[annotContainerName]; n != "" {
		return n
	}
	return e.Annotations[annotRefName]
}

// sameName dice si el nombre de una imagen del archivo es want. Por la
// referencia normalizada ("postgres:17" es "docker.io/library/postgres:17");
// si no se puede normalizar, tal cual.
func sameName(have, want string) bool {
	if have == want {
		return true
	}
	h, err1 := ParseImageRef(have)
	w, err2 := ParseImageRef(want)
	return err1 == nil && err2 == nil && h.String() == w.String()
}

// Image elige una imagen del archivo para linux/arch: la que se llama want
// (repo:etiqueta, o el digest de su manifiesto o su configuración) o, sin
// want, la única que haya.
func (a *Archive) Image(want, arch string) (*ArchiveImage, error) {
	if a.Format == FormatDockerArchive {
		return a.dockerImage(want, arch)
	}
	return a.ociImage(want, arch)
}

func (a *Archive) errChoose(want string) error {
	names := a.Names()
	list := strings.Join(names, ", ")
	if len(names) == 0 {
		list = "none are named"
	}
	if want == "" {
		return fmt.Errorf("the archive has several images; pick one with -image (names: %s)", list)
	}
	return fmt.Errorf("the archive has no image %q (names: %s)", want, list)
}

func (a *Archive) dockerImage(want, arch string) (*ArchiveImage, error) {
	var pick *dockerSaveEntry
	ref := ""
	for i, e := range a.docker {
		match := want == "" && len(a.docker) == 1
		for _, t := range e.RepoTags {
			if want != "" && sameName(t, want) {
				match, ref = true, t
			}
		}
		if want != "" && !match && reDigest.MatchString(want) {
			// Por el id de la imagen (el digest de su configuración).
			match = strings.TrimSuffix(path.Base(e.Config), ".json") == strings.TrimPrefix(want, "sha256:")
		}
		if match {
			if pick != nil {
				return nil, a.errChoose(want)
			}
			pick = &a.docker[i]
		}
	}
	if pick == nil {
		return nil, a.errChoose(want)
	}
	if ref == "" && len(pick.RepoTags) == 1 {
		ref = pick.RepoTags[0]
	}
	cfgBytes, err := a.read(pick.Config, maxConfig)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", pick.Config, err)
	}
	if err := checkPlatform(cfg, arch); err != nil {
		return nil, err
	}
	ids := cfg.RootFS.DiffIDs
	if len(pick.Layers) == 0 || len(ids) != len(pick.Layers) {
		return nil, fmt.Errorf("manifest.json has %d layers and the config %d diff_ids", len(pick.Layers), len(ids))
	}
	cfgDigest := sha(cfgBytes)
	img := &ArchiveImage{Format: FormatDockerArchive, Ref: ref, ConfigDigest: cfgDigest}
	m := ociManifest{SchemaVersion: 2, MediaType: MediaOCIManifest,
		Config: Descriptor{MediaType: MediaOCIConfig, Digest: cfgDigest, Size: int64(len(cfgBytes))}}
	var layers []ArchiveBlob
	seen := map[string]bool{}
	for i, l := range pick.Layers {
		if !reDigest.MatchString(ids[i]) {
			return nil, fmt.Errorf("config: invalid diff_id %q", ids[i])
		}
		n, err := a.fs.size(l)
		if err != nil {
			return nil, fmt.Errorf("layer %d: %w", i+1, err)
		}
		if err := a.plainTar(l); err != nil {
			return nil, fmt.Errorf("layer %d (%s): %w", i+1, l, err)
		}
		// La capa sin comprimir ES el diff_id: el daemon la comprueba con él.
		m.Layers = append(m.Layers, Descriptor{MediaType: MediaOCILayerTar, Digest: ids[i], Size: n})
		img.LayerBytes += n
		if !seen[ids[i]] {
			seen[ids[i]] = true
			layers = append(layers, ArchiveBlob{Digest: ids[i], Size: n, What: fmt.Sprintf("layer %d/%d", i+1, len(pick.Layers)), name: l})
		}
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	img.ManifestDigest = sha(mb)
	img.Blobs = append([]ArchiveBlob{
		{Digest: img.ManifestDigest, Size: int64(len(mb)), What: "manifest", data: mb},
		{Digest: cfgDigest, Size: int64(len(cfgBytes)), What: "config", data: cfgBytes},
	}, layers...)
	return img, nil
}

// ociManifest es el manifiesto que se escribe para un docker save.
type ociManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// plainTar comprueba que una capa de docker save va sin comprimir: su sha256
// tiene que ser el diff_id, y comprimida no lo sería nunca.
func (a *Archive) plainTar(name string) error {
	rc, err := a.fs.open(name)
	if err != nil {
		return err
	}
	defer rc.Close()
	var head [4]byte
	n, _ := io.ReadFull(rc, head[:])
	switch {
	case n >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return errors.New("compressed (gzip) layers in a docker save archive are not supported")
	case n == 4 && bytes.Equal(head[:], []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return errors.New("compressed (zstd) layers are not supported")
	}
	return nil
}

func checkPlatform(cfg Config, arch string) error {
	if cfg.Architecture != arch || (cfg.OS != "" && cfg.OS != "linux") {
		return fmt.Errorf("the image is %s/%s, not linux/%s", cfg.OS, cfg.Architecture, arch)
	}
	return nil
}

func (a *Archive) ociImage(want, arch string) (*ArchiveImage, error) {
	var pick *indexEntry
	for i, e := range a.index {
		match := want == "" && len(a.index) == 1
		if want != "" {
			if n := entryName(e); n != "" && sameName(n, want) {
				match = true
			} else if r := e.Annotations[annotRefName]; r != "" && e.Annotations[annotContainerName] == "" {
				// Solo la etiqueta: casa con la de want.
				if w, err := ParseImageRef(want); err == nil && w.Tag == r {
					match = true
				}
			}
			match = match || e.Digest == want
		}
		if match {
			if pick != nil {
				return nil, a.errChoose(want)
			}
			pick = &a.index[i]
		}
	}
	if pick == nil {
		return nil, a.errChoose(want)
	}
	ref := pick.Annotations[annotContainerName]
	if ref == "" {
		// Una ref.name es una referencia entera o solo la etiqueta: solo
		// la primera sirve de nombre.
		if r := pick.Annotations[annotRefName]; strings.ContainsAny(r, "/:") {
			ref = r
		}
	}
	if ref == "" && want != "" && !reDigest.MatchString(want) {
		ref = want
	}
	if ref != "" {
		if _, err := ParseImageRef(ref); err != nil {
			ref = ""
		}
	}
	d := pick.Descriptor
	var m Manifest
	var body []byte
	for depth := 0; ; depth++ {
		var err error
		if body, err = a.blobJSON(d); err != nil {
			return nil, err
		}
		if schema1(body, d.MediaType) {
			return nil, errSchema1(d.Digest)
		}
		m = Manifest{}
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("manifest %s: %w", d.Digest, err)
		}
		if len(m.Manifests) == 0 {
			break
		}
		if depth > 0 {
			return nil, fmt.Errorf("index %s: nested indexes are not supported", d.Digest)
		}
		p := pickPlatform(m.Manifests, arch)
		if p == nil {
			return nil, fmt.Errorf("the archive's image has no linux/%s manifest", arch)
		}
		d = *p
	}
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("manifest %s has no layers", d.Digest)
	}
	img := &ArchiveImage{Format: FormatOCILayout, Ref: ref, ManifestDigest: d.Digest, ConfigDigest: m.Config.Digest}
	img.Blobs = append(img.Blobs, ArchiveBlob{Digest: d.Digest, Size: int64(len(body)), What: "manifest", data: body})
	if m.Config.Size <= 0 || m.Config.Size > maxConfig {
		return nil, fmt.Errorf("config %s: declared size %d, want 1..%d bytes", m.Config.Digest, m.Config.Size, maxConfig)
	}
	cb, err := a.blobJSON(m.Config)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(cb, &cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", m.Config.Digest, err)
	}
	if err := checkPlatform(cfg, arch); err != nil {
		return nil, err
	}
	img.Blobs = append(img.Blobs, ArchiveBlob{Digest: m.Config.Digest, Size: int64(len(cb)), What: "config", data: cb})
	seen := map[string]bool{}
	for i, l := range m.Layers {
		if !reDigest.MatchString(l.Digest) {
			return nil, fmt.Errorf("layer %d: invalid digest %q", i+1, l.Digest)
		}
		if !strings.Contains(l.MediaType, "tar") || strings.Contains(l.MediaType, "zstd") {
			return nil, fmt.Errorf("layer %s: unsupported media type %q (only tar and tar+gzip)", l.Digest, l.MediaType)
		}
		n, err := a.fs.size(blobName(l.Digest))
		if err != nil {
			return nil, fmt.Errorf("layer %d (%s) is not in the archive (saved for another platform?): %w", i+1, l.Digest, err)
		}
		if n != l.Size {
			return nil, fmt.Errorf("layer %d (%s) is %d bytes, the manifest says %d", i+1, l.Digest, n, l.Size)
		}
		img.LayerBytes += n
		if !seen[l.Digest] {
			seen[l.Digest] = true
			img.Blobs = append(img.Blobs, ArchiveBlob{Digest: l.Digest, Size: n, What: fmt.Sprintf("layer %d/%d", i+1, len(m.Layers)), name: blobName(l.Digest)})
		}
	}
	return img, nil
}

// blobName es dónde está un blob en un layout OCI.
func blobName(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

// blobJSON lee un blob pequeño (índice, manifiesto, configuración) de un
// layout OCI y lo comprueba con su digest: se usa para elegir qué subir.
func (a *Archive) blobJSON(d Descriptor) ([]byte, error) {
	if !reDigest.MatchString(d.Digest) {
		return nil, fmt.Errorf("invalid digest %q", d.Digest)
	}
	max := int64(maxManifest)
	if d.Size > max {
		max = maxConfig
	}
	b, err := a.read(blobName(d.Digest), max)
	if err != nil {
		return nil, err
	}
	if got := sha(b); got != d.Digest {
		return nil, fmt.Errorf("blob %s: sha256 mismatch (got %s)", d.Digest, got)
	}
	if d.Size > 0 && int64(len(b)) != d.Size {
		return nil, fmt.Errorf("blob %s is %d bytes, the descriptor says %d", d.Digest, len(b), d.Size)
	}
	return b, nil
}

// Open abre un blob de la imagen para leerlo (y subirlo).
func (a *Archive) Open(b ArchiveBlob) (io.ReadCloser, error) {
	if b.data != nil {
		return io.NopCloser(bytes.NewReader(b.data)), nil
	}
	return a.fs.open(b.name)
}

// archivePath normaliza el nombre de una entrada: relativo, sin "..", sin
// NUL. "" es la raíz ("./").
func archivePath(n string) (string, error) {
	if strings.ContainsRune(n, 0) || path.IsAbs(n) {
		return "", fmt.Errorf("archive entry %q: absolute or invalid name", n)
	}
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return "", fmt.Errorf("archive entry %q: names with .. are not allowed", n)
		}
	}
	c := path.Clean(n)
	if c == "." {
		return "", nil
	}
	return c, nil
}

// tarEntry es una entrada del índice de un tar.
type tarEntry struct {
	off, size int64
	typ       byte
	link      string // a quién apunta un enlace, ya como nombre del archivo
}

// tarFS es un tar sin comprimir, indexado: las cabeceras se leen una vez
// (archive/tar salta los datos con Seek) y cada fichero se lee luego por su
// desplazamiento.
type tarFS struct {
	f    *os.File
	ents map[string]*tarEntry
}

func openTarFS(p string) (*tarFS, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	t := &tarFS{f: f, ents: map[string]*tarEntry{}}
	if err := t.index(); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *tarFS) index() error {
	var head [4]byte
	if n, _ := io.ReadFull(t.f, head[:]); n >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return errors.New("the archive is gzip-compressed: decompress it first (gunzip, or docker save without | gzip)")
	} else if n == 4 && bytes.Equal(head[:], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return errors.New("the archive is zstd-compressed: decompress it first")
	}
	if _, err := t.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tr := tar.NewReader(t.f)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		if n >= maxArchiveEntries {
			return fmt.Errorf("the archive has more than %d entries", maxArchiveEntries)
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return err
		}
		e := &tarEntry{typ: h.Typeflag, size: h.Size}
		switch h.Typeflag {
		case tar.TypeReg:
			for k := range h.PAXRecords {
				if strings.HasPrefix(k, "GNU.sparse.") {
					return fmt.Errorf("archive entry %q: sparse files are not supported", h.Name)
				}
			}
			// archive/tar no lee por delante: tras Next, el fichero está al
			// principio de los datos de la entrada.
			if e.off, err = t.f.Seek(0, io.SeekCurrent); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) {
				return fmt.Errorf("archive entry %q: symlink to an absolute path", h.Name)
			}
			target := path.Join(path.Dir(name), h.Linkname)
			if target == ".." || strings.HasPrefix(target, "../") {
				return fmt.Errorf("archive entry %q: symlink out of the archive", h.Name)
			}
			e.link = target
		case tar.TypeLink:
			if e.link, err = archivePath(h.Linkname); err != nil {
				return fmt.Errorf("archive entry %q: hard link: %w", h.Name, err)
			}
		case tar.TypeDir:
			continue
		case tar.TypeGNUSparse:
			return fmt.Errorf("archive entry %q: sparse files are not supported", h.Name)
		default:
			// Dispositivos, FIFOs...: no son blobs; si se piden, no existen.
			continue
		}
		if name == "" {
			continue
		}
		if _, dup := t.ents[name]; dup {
			return fmt.Errorf("archive entry %q appears twice", name)
		}
		t.ents[name] = e
	}
	return nil
}

// resolve sigue los enlaces internos hasta un fichero regular.
func (t *tarFS) resolve(name string) (*tarEntry, error) {
	orig := name
	name, err := archivePath(name)
	if err != nil {
		return nil, err
	}
	for hop := 0; hop <= maxLinkHops; hop++ {
		e := t.ents[name]
		if e == nil {
			return nil, fmt.Errorf("%s: not in the archive", orig)
		}
		if e.typ == tar.TypeReg {
			return e, nil
		}
		name = e.link
	}
	return nil, fmt.Errorf("%s: too many links", orig)
}

func (t *tarFS) size(name string) (int64, error) {
	e, err := t.resolve(name)
	if err != nil {
		return 0, err
	}
	return e.size, nil
}

func (t *tarFS) open(name string) (io.ReadCloser, error) {
	e, err := t.resolve(name)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.NewSectionReader(t.f, e.off, e.size)), nil
}

func (t *tarFS) close() error { return t.f.Close() }

// dirFS es un layout OCI en un directorio. Solo ficheros regulares: un
// enlace no se sigue, ni dentro del directorio.
type dirFS struct{ r *os.Root }

func (d *dirFS) lstat(name string) (os.FileInfo, error) {
	name, err := archivePath(name)
	if err != nil {
		return nil, err
	}
	fi, err := d.r.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("%s: not in the layout", name)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return fi, nil
}

func (d *dirFS) size(name string) (int64, error) {
	fi, err := d.lstat(name)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (d *dirFS) open(name string) (io.ReadCloser, error) {
	fi, err := d.lstat(name)
	if err != nil {
		return nil, err
	}
	f, err := d.r.Open(path.Clean(name))
	if err != nil {
		return nil, err
	}
	// Lo abierto tiene que ser lo comprobado: no un enlace puesto entre medias.
	if got, err := f.Stat(); err != nil || !os.SameFile(fi, got) {
		f.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", name)
	}
	return f, nil
}

func (d *dirFS) close() error { return d.r.Close() }
