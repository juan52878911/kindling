package imagen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/internal/durable"
)

// FetchVerified deja en dst el fichero de url con ese sha256 (y tamaño, si
// no es 0). Si dst ya está y cuadra, no baja nada; escribe al lado y renombra.
func FetchVerified(ctx context.Context, url, want string, size int64, dst string) error {
	if len(want) != 64 {
		return fmt.Errorf("%s: no sha256 to check it against", url)
	}
	if got, err := digest.File(dst); err == nil && got == want {
		return nil
	}
	return fetch(ctx, url, want, size, dst)
}

// fetch baja url a dst (escribe al lado y renombra). Con want comprueba el
// sha256; sin él, solo que no pase de size (o de 1 GiB).
func fetch(ctx context.Context, url, want string, size int64, dst string) error {
	// 10 minutos, o lo que tarde el fichero a 1 MiB/s si es más (la imagen
	// del emulador de arm_translation "libndk" son 1,4 GiB).
	timeout := max(10*time.Minute, time.Duration(size>>20)*time.Second)
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	limit := size
	if limit <= 0 {
		limit = 1 << 30
	}
	// Temporal propio, sha256 y tamaño comprobados, en disco antes del rename
	// (antes no había fsync: un corte dejaba el nombre bueno con medio fichero,
	// que el siguiente FetchVerified rehasheaba, pero los pins sin hash no).
	o := durable.Opciones{SHA256: want, Max: limit, Patron: filepath.Base(dst) + ".part-*"}
	if want != "" {
		// Sin hash, size es solo un tope (los índices del resolver).
		o.Size = size
	}
	t, err := durable.Recibir(filepath.Dir(dst), resp.Body, o)
	if err != nil {
		var et *durable.ErrTamaño
		var eh *durable.ErrSHA256
		var ed *durable.ErrDemasiado
		switch {
		case errors.As(err, &ed):
			return fmt.Errorf("%s: over %d bytes", url, limit)
		case errors.As(err, &et):
			return fmt.Errorf("%s: %d bytes, expected %d", url, et.N, et.Quiero)
		case errors.As(err, &eh):
			return fmt.Errorf("%s: sha256 mismatch (got %s)", url, eh.Got)
		}
		return err
	}
	return t.Publicar(dst)
}

// SHA256Hex es el sha256 de b, en hexadecimal.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
