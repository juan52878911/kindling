// Package upgrade cambia los binarios de una instalación de kindling por los de
// otra release sin dejarla a medias: baja y verifica todo antes de tocar nada,
// comprueba que el binario nuevo entiende el estado que va a encontrar, guarda
// los binarios de ahora, para el daemon, cambia, arranca, verifica y, si algo
// de eso último falla, vuelve solo a lo de antes. Es `kling upgrade`
// (docs/actualizar.md §3.4).
//
// Aquí está el flujo y lo que se puede probar sin un host: de dónde salen los
// binarios (Fuente), qué se cambia (Pieza) y cómo se para y se arranca el
// daemon (Servicio, que en Linux es systemd y en macOS launchd). Lo que es de
// cada sistema lo pone cmd/kling.
package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/juan52878911/kindling/pkg/lazyre"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// Repo es de dónde sale una release por defecto. Los assets están en
// <Repo>/releases/download/<tag>/<asset>, con un SHA256SUMS por release.
const Repo = "https://github.com/juan52878911/kindling"

// Límites de lo que se lee de fuera: un binario de kling ronda las decenas de
// MiB; 512 MiB deja margen sin dejar que un servidor llene el disco.
const (
	maxAsset = 512 << 20
	maxSums  = 1 << 20
)

// reEtiqueta es una etiqueta de release: vX.Y.Z con, como mucho, un sufijo
// de prerelease. Va en una URL y en una ruta: nada de / ni de "..".
var reEtiqueta = lazyre.New(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// EtiquetaValida dice si t sirve como etiqueta de release.
func EtiquetaValida(t string) bool { return reEtiqueta.MatchString(t) && !strings.Contains(t, "..") }

// Fuente es de dónde salen los binarios: una release (Repo) o un directorio
// local con los mismos nombres (Dir, el -from-dir de `kling upgrade`).
type Fuente struct {
	Repo   string // vacío = Repo
	Dir    string
	Client *http.Client

	// permitirHTTP deja bajar por http:// en los tests. No se puede activar
	// desde fuera: fuera de un test, http es un binario que cualquiera en el
	// camino puede cambiar.
	permitirHTTP bool
}

func (f *Fuente) repo() string {
	if f.Repo == "" {
		return Repo
	}
	return strings.TrimSuffix(f.Repo, "/")
}

func (f *Fuente) comprobarEsquema(u *url.URL) error {
	if u.Scheme == "https" || (f.permitirHTTP && u.Scheme == "http") {
		return nil
	}
	return fmt.Errorf("refusing %s: releases are only downloaded over https", u.Redacted())
}

// cliente es el de las descargas: con plazo y sin seguir una redirección que
// no sea https (GitHub manda los assets a otro dominio, siempre por https).
func (f *Fuente) cliente(seguir bool) *http.Client {
	base := f.Client
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Minute}
	}
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !seguir {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return f.comprobarEsquema(req.URL)
	}
	return &c
}

// UltimaEtiqueta pregunta cuál es la última release estable: GitHub redirige
// <repo>/releases/latest a <repo>/releases/tag/<etiqueta>. Es lo mismo que
// hace install.sh, sin depender del API (y de su límite de peticiones).
func (f *Fuente) UltimaEtiqueta(ctx context.Context) (string, error) {
	u, err := url.Parse(f.repo() + "/releases/latest")
	if err != nil {
		return "", err
	}
	if err := f.comprobarEsquema(u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := f.cliente(false).Do(req)
	if err != nil {
		return "", fmt.Errorf("latest release: %v", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("latest release: %s answered %s without a redirect; pass -tag vX.Y.Z", u.Redacted(), resp.Status)
	}
	t := path.Base(strings.TrimSuffix(loc, "/"))
	if !EtiquetaValida(t) {
		return "", fmt.Errorf("latest release: unexpected redirect to %q; pass -tag vX.Y.Z", loc)
	}
	return t, nil
}

// Bajar deja en dest cada asset de la release etiqueta (o de Dir), verificado:
// devuelve la ruta de cada uno. Un asset remoto que no está en SHA256SUMS, o
// cuyo hash no coincide, es un error y no queda nada suyo en dest. En Dir, el
// SHA256SUMS es opcional (los ficheros ya están en esta máquina), pero si
// existe cada asset tiene que estar en él y coincidir. Se acepta el nombre
// del asset (kling-linux-amd64) o el del binario a secas (kling).
func (f *Fuente) Bajar(ctx context.Context, etiqueta string, assets []Asset, dest string) (map[string]string, error) {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return nil, err
	}
	sumas, err := f.sumas(ctx, etiqueta)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range assets {
		p, err := f.bajarUno(ctx, etiqueta, a, sumas, dest)
		if err != nil {
			return nil, err
		}
		out[a.Nombre] = p
	}
	return out, nil
}

// Asset es un fichero de la release y, para un directorio local, el nombre a
// secas con el que también puede estar (kling-guest en vez de
// kling-guest-linux-amd64).
type Asset struct {
	Nombre string
	Corto  string
}

// sumas lee el SHA256SUMS de la release o de Dir. nil sin error = Dir sin él.
func (f *Fuente) sumas(ctx context.Context, etiqueta string) (map[string]string, error) {
	if f.Dir != "" {
		b, err := leerLimitado(filepath.Join(f.Dir, "SHA256SUMS"), maxSums)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return plugin.ParseSums(b), nil
	}
	if !EtiquetaValida(etiqueta) {
		return nil, fmt.Errorf("invalid release tag %q", etiqueta)
	}
	tmp, err := os.CreateTemp("", "kling-sums-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := f.descargar(ctx, f.urlAsset(etiqueta, "SHA256SUMS"), tmp, maxSums); err != nil {
		return nil, fmt.Errorf("SHA256SUMS: %v", err)
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	return plugin.ParseSums(b), nil
}

func (f *Fuente) urlAsset(etiqueta, nombre string) string {
	return f.repo() + "/releases/download/" + etiqueta + "/" + nombre
}

func (f *Fuente) bajarUno(ctx context.Context, etiqueta string, a Asset, sumas map[string]string, dest string) (string, error) {
	final := filepath.Join(dest, a.Nombre)
	tmp, err := os.CreateTemp(dest, "."+a.Nombre+".part-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	var got, listado string
	var listadoOK bool
	if f.Dir != "" {
		origen := filepath.Join(f.Dir, a.Nombre)
		nombre := a.Nombre
		if _, err := os.Stat(origen); os.IsNotExist(err) && a.Corto != "" {
			origen, nombre = filepath.Join(f.Dir, a.Corto), a.Corto
		}
		src, err := os.Open(origen)
		if err != nil {
			return "", fmt.Errorf("%s: %v", a.Nombre, err)
		}
		got, err = copiarConHash(tmp, src, maxAsset, origen)
		src.Close()
		if err != nil {
			return "", err
		}
		listado, listadoOK = sumas[a.Nombre]
		if !listadoOK {
			listado, listadoOK = sumas[nombre]
		}
		if sumas != nil && !listadoOK {
			return "", fmt.Errorf("%s: not listed in %s, refusing it unverified", a.Nombre, filepath.Join(f.Dir, "SHA256SUMS"))
		}
	} else {
		got, err = f.descargar(ctx, f.urlAsset(etiqueta, a.Nombre), tmp, maxAsset)
		if err != nil {
			return "", fmt.Errorf("%s: %v", a.Nombre, err)
		}
		listado, listadoOK = sumas[a.Nombre]
		if !listadoOK {
			return "", fmt.Errorf("%s: not listed in the release's SHA256SUMS, refusing it unverified", a.Nombre)
		}
	}
	if listadoOK && got != listado {
		return "", fmt.Errorf("%s: sha256 mismatch: got %s, SHA256SUMS lists %s", a.Nombre, got, listado)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// descargar escribe u en w y devuelve su sha256. Más de limite bytes es un
// error, no un truncado: un binario cortado tendría otro hash y despistaría.
func (f *Fuente) descargar(ctx context.Context, u string, w io.Writer, limite int64) (string, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	if err := f.comprobarEsquema(pu); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.cliente(true).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", pu.Redacted(), resp.Status)
	}
	return copiarConHash(w, resp.Body, limite, pu.Redacted())
}

func copiarConHash(w io.Writer, r io.Reader, limite int64, que string) (string, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(r, limite+1))
	if err != nil {
		return "", err
	}
	if n > limite {
		return "", fmt.Errorf("%s is larger than %d MiB", que, limite>>20)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func leerLimitado(p string, limite int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limite+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limite {
		return nil, fmt.Errorf("%s is larger than %d MiB", p, limite>>20)
	}
	return b, nil
}
