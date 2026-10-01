package imagen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// FetchVerified deja en dst el fichero de url con ese sha256 (y tamaño, si
// no es 0). Si dst ya está y cuadra, no baja nada; escribe al lado y renombra.
func FetchVerified(ctx context.Context, url, want string, size int64, dst string) error {
	if len(want) != 64 {
		return fmt.Errorf("%s: no sha256 to check it against", url)
	}
	if got, err := SHA256File(dst); err == nil && got == want {
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
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	defer f.Close()
	limit := size
	if limit <= 0 {
		limit = 1 << 30
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if want == "" {
		if n > limit {
			return fmt.Errorf("%s: over %d bytes", url, limit)
		}
	} else if size > 0 && n != size {
		return fmt.Errorf("%s: %d bytes, expected %d", url, n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && got != want {
		return fmt.Errorf("%s: sha256 mismatch (got %s)", url, got)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// SHA256File es el sha256 de un fichero, en hexadecimal.
func SHA256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SHA256Hex es el sha256 de b, en hexadecimal.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
