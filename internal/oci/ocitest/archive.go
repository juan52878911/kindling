package ocitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/zstd"
)

// Saved es una imagen para DockerSave y OCILayout.
type Saved struct {
	// Tags son sus nombres ("postgres:17-alpine").
	Tags []string
	Arch string
	// Config es la parte "config" de la configuración (Entrypoint, Cmd...).
	Config map[string]any
	// Layers son las capas: tars sin comprimir para DockerSave; para
	// OCILayout, sin comprimir, con gzip o con zstd.
	Layers [][]byte
}

// SavedConfig es el JSON de la configuración de s, con los diff_ids (el
// sha256 de cada capa sin comprimir).
func SavedConfig(s Saved) []byte {
	var ids []string
	for _, l := range s.Layers {
		ids = append(ids, digest(plain(l)))
	}
	b, _ := json.Marshal(map[string]any{"architecture": s.Arch, "os": "linux", "config": s.Config,
		"rootfs": map[string]any{"type": "layers", "diff_ids": ids}})
	return b
}

// plain descomprime una capa con gzip o zstd; una sin comprimir, tal cual.
func plain(l []byte) []byte {
	var r io.Reader
	switch {
	case esZstd(l):
		r = zstd.NewReader(bytes.NewReader(l))
	case len(l) >= 2 && l[0] == 0x1f && l[1] == 0x8b:
		zr, err := gzip.NewReader(bytes.NewReader(l))
		if err != nil {
			panic(err)
		}
		r = zr
	default:
		return l
	}
	b, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return b
}

func esZstd(l []byte) bool { return bytes.HasPrefix(l, []byte{0x28, 0xb5, 0x2f, 0xfd}) }

// DockerSave es lo que deja `docker save` hasta la versión 24: por imagen,
// <id>.json (la configuración), <capa>/layer.tar sin comprimir, manifest.json
// y repositories. Una capa que ya salió (de otra imagen, o repetida) es un
// enlace simbólico al primer layer.tar, como hace docker.
func DockerSave(imgs ...Saved) map[string][]byte {
	files := map[string][]byte{}
	var manifest []map[string]any
	repos := map[string]map[string]string{}
	first := map[string]string{}
	for n, s := range imgs {
		cfg := SavedConfig(s)
		id := strings.TrimPrefix(digest(cfg), "sha256:")
		files[id+".json"] = cfg
		var layers []string
		for i, l := range s.Layers {
			d := strings.TrimPrefix(digest(l), "sha256:")
			dir := fmt.Sprintf("%s%02d%02d", d[:40], n, i)
			name := dir + "/layer.tar"
			if prev, ok := first[d]; ok {
				files[name] = []byte("->../" + prev)
			} else {
				first[d] = name
				files[name] = l
				files[dir+"/VERSION"] = []byte("1.0")
			}
			layers = append(layers, name)
		}
		manifest = append(manifest, map[string]any{"Config": id + ".json", "RepoTags": s.Tags, "Layers": layers})
		for _, t := range s.Tags {
			repo, tag, _ := strings.Cut(t, ":")
			if repos[repo] == nil {
				repos[repo] = map[string]string{}
			}
			repos[repo][tag] = id
		}
	}
	files["manifest.json"], _ = json.Marshal(manifest)
	files["repositories"], _ = json.Marshal(repos)
	return files
}

// OCILayout es un layout OCI (docker save desde la versión 25, o skopeo):
// index.json con una entrada por nombre de imagen (anotaciones de docker),
// oci-layout y blobs/sha256/. Devuelve también el digest del manifiesto de
// cada imagen.
func OCILayout(imgs ...Saved) (map[string][]byte, []string) {
	files := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)}
	put := func(b []byte) string {
		d := digest(b)
		files["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")] = b
		return d
	}
	entries := []map[string]any{}
	var manifests []string
	for _, s := range imgs {
		cfg := SavedConfig(s)
		var ls []map[string]any
		for _, l := range s.Layers {
			mt := "application/vnd.oci.image.layer.v1.tar"
			if esZstd(l) {
				mt += "+zstd"
			} else if !bytes.Equal(plain(l), l) {
				mt += "+gzip"
			}
			ls = append(ls, map[string]any{"mediaType": mt, "digest": put(l), "size": len(l)})
		}
		m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": put(cfg), "size": len(cfg)},
			"layers": ls})
		md := put(m)
		manifests = append(manifests, md)
		for _, t := range s.Tags {
			_, tag, _ := strings.Cut(t, ":")
			entries = append(entries, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest": md, "size": len(m), "annotations": map[string]string{
					"io.containerd.image.name": "docker.io/library/" + t, "org.opencontainers.image.ref.name": tag}})
		}
		if len(s.Tags) == 0 {
			entries = append(entries, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest": md, "size": len(m)})
		}
	}
	files["index.json"], _ = json.Marshal(map[string]any{"schemaVersion": 2, "manifests": entries})
	return files, manifests
}

// TarFiles empaqueta files en un tar, por orden de nombre. Un contenido que
// empieza por "->" es un enlace simbólico a lo que sigue.
func TarFiles(files map[string][]byte) []byte {
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		h := &tar.Header{Name: n, Mode: 0o644, ModTime: time.Unix(1700000000, 0)}
		b := files[n]
		if l, ok := bytes.CutPrefix(b, []byte("->")); ok {
			h.Typeflag, h.Linkname = tar.TypeSymlink, string(l)
			b = nil
		} else {
			h.Typeflag, h.Size = tar.TypeReg, int64(len(b))
		}
		tw.WriteHeader(h)
		tw.Write(b)
	}
	tw.Close()
	return buf.Bytes()
}

// WriteDir deja files en dir (los enlaces, como enlaces).
func WriteDir(dir string, files map[string][]byte) error {
	for n, b := range files {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if l, ok := bytes.CutPrefix(b, []byte("->")); ok {
			if err := os.Symlink(string(l), p); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
