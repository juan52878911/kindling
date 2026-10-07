package oci

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/juan52878911/kindling/internal/zstd"
)

// compressed dice si el media type es el de una capa comprimida (tar+gzip,
// tar+zstd, o los de Docker: tar.gzip...).
func compressed(mediaType string) bool {
	return strings.Contains(mediaType, "gzip") || strings.Contains(mediaType, "zstd")
}

type layerFile struct {
	io.Reader
	f *os.File
}

func (l *layerFile) Close() error { return l.f.Close() }

const zstdMagic = "\x28\xb5\x2f\xfd"

// isZstd dice si el fichero empieza como un marco zstd.
func isZstd(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var m [4]byte
	_, err = io.ReadFull(f, m[:])
	return err == nil && string(m[:]) == zstdMagic
}

// decompress abre una capa comprimida con gzip o zstd. Lo deciden los
// primeros bytes, no el media type: hay herramientas que suben capas zstd
// etiquetadas como gzip. Con kling-unpack instalado lo hace él (ver
// unpack_helper.go); si no, Go.
func decompress(f *os.File) (io.ReadCloser, error) {
	if h := unpackHelper(); h != "" {
		rc, err := decompressHelper(h, f)
		if err == nil {
			return rc, nil
		}
		// No arrancó: f sigue sin leer, Go lo abre desde el principio.
		warnHelper(h, err)
	}
	br := bufio.NewReaderSize(f, 1<<20)
	m, _ := br.Peek(4)
	var r io.Reader
	var err error
	switch {
	case len(m) == 4 && m[0] == 0x1f && m[1] == 0x8b:
		r, err = gzip.NewReader(br)
	case string(m) == zstdMagic:
		r = zstd.NewReader(br)
	default:
		err = errors.New("compressed layer is neither gzip nor zstd")
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return &layerFile{r, f}, nil
}
