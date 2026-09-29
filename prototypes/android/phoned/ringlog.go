package main

import (
	"bytes"
	"io"
	"sync"
)

// ringLog guarda los últimos max bytes de lo que escribe kling-phoned (y la
// salida del hijo antes del exec de /init) para GET /v1/logs?buffer=phoned. La
// copia completa sigue yendo a /var/log/service.log.
type ringLog struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

func (r *ringLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = append(r.buf[:0:0], r.buf[over:]...)
	}
	return len(p), nil
}

// tail devuelve las últimas n líneas.
func (r *ringLog) tail(n int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.buf
	i := len(b)
	if i > 0 && b[i-1] == '\n' {
		i--
	}
	for ; n > 0 && i > 0; n-- {
		j := bytes.LastIndexByte(b[:i], '\n')
		if j < 0 {
			i = 0
			break
		}
		i = j
	}
	if i > 0 {
		i++
	}
	return append([]byte(nil), b[i:]...)
}

// tee escribe en w y en el anillo.
func (r *ringLog) tee(w io.Writer) io.Writer { return io.MultiWriter(w, r) }
