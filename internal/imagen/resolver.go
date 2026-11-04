package imagen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/internal/deb"
	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/xz"
	"github.com/juan52878911/kindling/pkg/lazyre"
)

// RESOLVER PAQUETES CONTRA UN SNAPSHOT. El constructor debian añade paquetes
// que no están en el lock de la base: los resuelve contra los índices de
// snapshot.debian.org del MISMO instante en que se fijó la base (así lo que
// añade casa con las bibliotecas que ya hay) y apunta cada .deb con su
// sha256 en un lockfile. Con ese lockfile en la receta no se resuelve nada:
// se baja lo fijado y se comprueba.
//
// El índice (Packages.xz) se baja por HTTPS sin comprobar la firma de
// Release, como lockgen: lo que queda fijado y se revisa es el sha256 de cada
// .deb, que va en la receta.

// IndexSource es un índice Packages.xz y el archivo al que apuntan sus rutas.
type IndexSource struct {
	URL  string // el Packages.xz (o Packages, sin comprimir)
	Base string // donde están sus Filename (las URL de los DebPin)
}

// IndexSources son los índices de trixie, trixie-updates y trixie-security
// del snapshot para arch. Es una variable para las pruebas.
var IndexSources = func(snapshot, arch string) []IndexSource {
	var out []IndexSource
	for _, s := range []struct{ archive, base, suite string }{
		{"debian", "https://deb.debian.org/debian", "trixie"},
		{"debian", "https://deb.debian.org/debian", "trixie-updates"},
		{"debian-security", "https://security.debian.org/debian-security", "trixie-security"},
	} {
		out = append(out, IndexSource{
			URL:  fmt.Sprintf("https://snapshot.debian.org/archive/%s/%s/dists/%s/main/binary-%s/Packages.xz", s.archive, snapshot, s.suite, arch),
			Base: s.base,
		})
	}
	return out
}

var reSnapshot = lazyre.New(`^[0-9]{8}T[0-9]{6}Z$`)

// maxIndex es el tope de un Packages.xz (el de trixie/main son ~10 MiB).
const maxIndex = 128 << 20

// Resolve resuelve want contra los índices del snapshot de d, sin lo que ya
// está instalado en la base (su status). Los índices se guardan en la caché
// (un snapshot no cambia).
func (d Debs) Resolve(ctx context.Context, arch string, status []byte, want []string) ([]DebPin, error) {
	if !reSnapshot.MatchString(d.Snapshot) {
		return nil, fmt.Errorf("invalid snapshot %q", d.Snapshot)
	}
	installed, err := deb.Installed(bytes.NewReader(status))
	if err != nil {
		return nil, fmt.Errorf("dpkg status: %w", err)
	}
	ix := deb.NewIndex()
	dir := filepath.Join(d.Cache, "debian-index", d.Snapshot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, src := range IndexSources(d.Snapshot, arch) {
		h := sha256.Sum256([]byte(src.URL))
		p := filepath.Join(dir, hex.EncodeToString(h[:8])+"."+path.Base(src.URL))
		if _, err := os.Stat(p); err != nil {
			d.logf("index: %s", src.URL)
			// snapshot.debian.org a veces tarda en dar la mano: tres intentos.
			var err error
			for try := 1; try <= 3; try++ {
				if err = fetch(ctx, src.URL, "", maxIndex, p); err == nil {
					break
				}
				d.logf("index: try %d: %v", try, err)
			}
			if err != nil {
				return nil, err
			}
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r io.Reader = bytes.NewReader(raw)
		if strings.HasSuffix(src.URL, ".xz") {
			r, err = xz.NewReader(r)
		}
		if err == nil {
			err = ix.Add(r, src.Base)
		}
		if err != nil {
			os.Remove(p) // que la próxima vez se vuelva a bajar
			return nil, fmt.Errorf("%s: %w", src.URL, err)
		}
	}
	sel, err := ix.Resolve(want, installed)
	if err != nil {
		return nil, err
	}
	pins := make([]DebPin, 0, len(sel))
	for _, p := range sel {
		pin := DebPin{Name: p.Name, Version: p.Version, URL: p.Base + "/" + p.Filename, SHA256: p.SHA256, Size: p.Size}
		if err := CheckPin(pin); err != nil {
			return nil, fmt.Errorf("index entry: %w", err)
		}
		pins = append(pins, pin)
	}
	return pins, nil
}

var (
	rePkgName = lazyre.New(`^[a-z0-9][a-z0-9+.-]{0,127}$`)
	reHex64   = lazyre.New(`^[0-9a-f]{64}$`)
)

// PinHosts son los únicos sitios de los que el constructor baja un .deb de
// un lockfile: corre como root en el host del daemon, y una URL cualquiera
// en una receta sería una petición a la red interna.
var PinHosts = []string{
	"https://deb.debian.org/debian/",
	"https://security.debian.org/debian-security/",
	"https://snapshot.debian.org/archive/",
}

// CheckPin valida una línea del lockfile.
func CheckPin(p DebPin) error {
	if !rePkgName.MatchString(p.Name) {
		return fmt.Errorf("invalid package name %q", p.Name)
	}
	if p.Version == "" || len(p.Version) > 128 || strings.ContainsAny(p.Version, " \t\r\n\x00") {
		return fmt.Errorf("%s: invalid version %q", p.Name, p.Version)
	}
	if !reHex64.MatchString(p.SHA256) {
		return fmt.Errorf("%s: sha256 must be 64 hex digits", p.Name)
	}
	if p.Size <= 0 || p.Size > 1<<30 {
		return fmt.Errorf("%s: invalid size %d", p.Name, p.Size)
	}
	ok := false
	for _, h := range PinHosts {
		ok = ok || strings.HasPrefix(p.URL, h)
	}
	if !ok || strings.Contains(p.URL, "/../") || strings.ContainsAny(p.URL, "?#\\ \r\n\x00") || !strings.HasSuffix(p.URL, ".deb") {
		return fmt.Errorf("%s: url must be a .deb on deb.debian.org, security.debian.org or snapshot.debian.org", p.Name)
	}
	return nil
}

// StatusOf es el /var/lib/dpkg/status de un árbol (nil si no lo tiene).
func StatusOf(root *ext4.Node) []byte {
	if n := root.Lookup("/var/lib/dpkg/status"); n != nil {
		if data, ok := n.Data.(ext4.Bytes); ok {
			return append([]byte{}, data...)
		}
	}
	return nil
}
