package oci

import (
	"bufio"
	"compress/gzip"
	"io"
	"os"
)

type gzipFile struct {
	*gzip.Reader
	f *os.File
}

func (g *gzipFile) Close() error {
	g.Reader.Close()
	return g.f.Close()
}

func newGzip(f *os.File) (io.ReadCloser, error) {
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &gzipFile{zr, f}, nil
}
