package oci

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// EL CAMINO RÁPIDO DE LA DESCOMPRESIÓN. compress/gzip va a ~200 MB/s y el
// decodificador zstd propio a ~300; en un import grande es lo que más tarda.
// kling-unpack (rust/kling-unpack: zlib-rs y libzstd) hace lo mismo 2,2× más
// rápido en gzip y 3× en zstd (medido en el lab con la capa de 570 MiB de
// node:22). El núcleo sigue sin cgo: es un proceso aparte, como Firecracker,
// y si no está se descomprime en Go igual que siempre.
//
// La capa ya llega verificada por sha256; el ayudante solo la abre, y su
// salida solo cuenta si sale con 0 (el CRC32 de gzip y el xxhash64 de zstd los
// comprueba él al llegar al final, como Go).

var (
	helperMu   sync.Mutex
	helperPath string // fijado con SetUnpackHelper; "" = el de por defecto
	helperWarn sync.Once
)

// SetUnpackHelper fija dónde está kling-unpack (el constructor lo busca en su
// KLING_LIB_DIR, que en macOS no es el de Linux). KLING_UNPACK manda sobre esto.
func SetUnpackHelper(path string) {
	helperMu.Lock()
	helperPath = path
	helperMu.Unlock()
}

// UnpackHelper dice con qué se descomprimen las capas: la ruta de
// kling-unpack, o "" si es en Go.
func UnpackHelper() string { return unpackHelper() }

// unpackHelper devuelve la ruta del ayudante que usar, o "" para Go.
// KLING_UNPACK=0 (o vacío) lo apaga; con una ruta, se usa esa.
func unpackHelper() string {
	p, set := os.LookupEnv("KLING_UNPACK")
	switch {
	case set && (p == "" || p == "0"):
		return ""
	case !set:
		helperMu.Lock()
		p = helperPath
		helperMu.Unlock()
		if p == "" {
			lib := os.Getenv("KLING_LIB_DIR")
			if lib == "" {
				lib = "/usr/local/lib/kindling"
			}
			p = filepath.Join(lib, "kling-unpack")
		}
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return p
}

// decompressHelper descomprime f con el ayudante: f va como su stdin (el
// descriptor tal cual, sin copias) y lo descomprimido se lee de su stdout.
func decompressHelper(path string, f *os.File) (io.ReadCloser, error) {
	cmd := exec.Command(path)
	cmd.Stdin = f
	cmd.Env = []string{} // nada del entorno del constructor le hace falta
	errb := &tailBuffer{max: 2048}
	cmd.Stderr = errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &helperReader{cmd: cmd, out: out, f: f, errb: errb}, nil
}

// helperReader da io.EOF solo cuando el ayudante salió con 0: si muere a mitad
// (capa dañada, sin memoria), lo leído hasta ahí no se toma por la capa entera.
type helperReader struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	f      *os.File
	errb   *tailBuffer
	waited bool
	err    error
}

func (h *helperReader) Read(p []byte) (int, error) {
	if h.waited {
		if h.err != nil {
			return 0, h.err
		}
		return 0, io.EOF
	}
	n, err := h.out.Read(p)
	if err == io.EOF {
		h.wait()
		if h.err != nil {
			return n, h.err
		}
	}
	return n, err
}

func (h *helperReader) wait() {
	h.waited = true
	if err := h.cmd.Wait(); err != nil {
		msg := strings.TrimSpace(strings.TrimPrefix(h.errb.String(), "kling-unpack: "))
		if msg == "" {
			msg = err.Error()
		}
		h.err = fmt.Errorf("decompressing layer: %s", msg)
	}
}

// Close para el ayudante si no ha acabado (quien lee lo deja a medias: otra
// capa falló o se canceló) y cierra la capa.
func (h *helperReader) Close() error {
	if !h.waited {
		h.waited = true
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
		if h.err == nil {
			h.err = errors.New("decompressing layer: closed before the end")
		}
	}
	return h.f.Close()
}

// tailBuffer guarda lo último que escribió el ayudante en stderr, con tope.
type tailBuffer struct {
	mu  sync.Mutex
	b   bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b.Write(p)
	if extra := t.b.Len() - t.max; extra > 0 {
		t.b.Next(extra)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}

// warnHelper avisa una vez de que el ayudante está pero no arranca (otra
// arquitectura, sin permisos): se sigue en Go, más lento.
func warnHelper(path string, err error) {
	helperWarn.Do(func() {
		log.Printf("warning: %s does not start (%v): decompressing layers in Go", path, err)
	})
}
