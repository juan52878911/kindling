package imagen

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/juan52878911/kindling/internal/deb"
	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/pkg/api"
)

// IMÁGENES EMULADAS (docs/imagenes-amd64.md).
//
// Una imagen OCI que solo existe para amd64 corre en un anfitrión arm64 con
// qemu-user DENTRO del invitado: el kernel y el agente son arm64, y
// binfmt_misc manda cada ELF x86-64 a qemu-x86_64. El constructor oci mete el
// binario de qemu (estático, de Debian, fijado por sha256 del .deb y del
// propio binario) y la marca api.GuestEmulateFile; el init del agente lo
// registra.

// QemuPin es un qemu-user fijado: el .deb de Debian que lo trae, el miembro de
// data.tar y el sha256 y tamaño del binario.
type QemuPin struct {
	Deb      DebPin
	Snapshot string // marca de snapshot.debian.org donde el .deb ya estaba
	Member   string // "./usr/bin/qemu-x86_64"
	SHA256   string
	Size     int64
}

// qemuPins es [anfitrión][invitado]. Debian 13 (trixie): desde qemu 10 el
// paquete qemu-user ya es estático (qemu-user-static solo es de transición).
// Para cambiar de versión: el Packages de trixie da Filename, Size y SHA256
// del .deb (sha256sum después de bajarlo para comprobar), y
// `ar p qemu-user_*.deb data.tar.xz | tar -xJO ./usr/bin/qemu-x86_64 | sha256sum`
// el del binario; first_seen en snapshot.debian.org/mr/file/<hash>/info.
var qemuPins = map[string]map[string]QemuPin{
	"arm64": {
		"amd64": {
			Deb: DebPin{Name: "qemu-user", Version: "1:10.0.13+ds-0+deb13u1",
				URL:    "https://deb.debian.org/debian/pool/main/q/qemu/qemu-user_10.0.13+ds-0+deb13u1_arm64.deb",
				SHA256: "c73711af02b97cd5e2667e735d9c06890c57165517110ca4e646c95a7083158d", Size: 64178608},
			Snapshot: "20260905T205002Z",
			Member:   "./usr/bin/qemu-x86_64",
			SHA256:   "9af86352fd463ad9ec25bdd6a4b891ac6b6a7f3d46a7db3da6f23e2c09094a59",
			Size:     14008992,
		},
	},
}

// Emulable dice si en un anfitrión host se puede correr una imagen de guest
// con qemu-user, y con cuál.
func Emulable(host, guest string) (QemuPin, bool) {
	p, ok := qemuPins[host][guest]
	if ok {
		_, ok = api.GuestEmulators[guest]
	}
	return p, ok
}

// Emulador es el qemu-user que va en la imagen.
type Emulador struct {
	Guest  string // la arquitectura emulada
	Bin    []byte
	SHA256 string
}

// Qemu baja (o saca de la caché) el .deb de pin, extrae el binario y lo
// comprueba: sha256 y tamaño fijados, y ELF de la arquitectura host (el
// intérprete corre nativo).
func (d Debs) Qemu(ctx context.Context, pin QemuPin, host, guest string) (*Emulador, error) {
	if d.Snapshot == "" {
		d.Snapshot = pin.Snapshot
	}
	f, err := d.Fetch(ctx, pin.Deb)
	if err != nil {
		return nil, err
	}
	dp, err := deb.Open(f)
	if err != nil {
		return nil, err
	}
	rc, err := dp.Data()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pin.Deb.Name, err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s %s has no %s", pin.Deb.Name, pin.Deb.Version, pin.Member)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pin.Deb.Name, err)
		}
		if h.Name != pin.Member {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size != pin.Size {
			return nil, fmt.Errorf("%s in %s: not a %d-byte regular file", pin.Member, pin.Deb.Name, pin.Size)
		}
		b := make([]byte, h.Size)
		if _, err := io.ReadFull(tr, b); err != nil {
			return nil, fmt.Errorf("%s: %w", pin.Member, err)
		}
		s := sha256.Sum256(b)
		if got := hex.EncodeToString(s[:]); got != pin.SHA256 {
			return nil, fmt.Errorf("%s: sha256 %s, want %s", pin.Member, got, pin.SHA256)
		}
		if err := CheckELF(b, host); err != nil {
			return nil, fmt.Errorf("%s: %w", pin.Member, err)
		}
		return &Emulador{Guest: guest, Bin: b, SHA256: pin.SHA256}, nil
	}
}

// PutEmulador mete el intérprete y la marca de emulación en root.
func PutEmulador(root *ext4.Node, e *Emulador, t time.Time) error {
	p, ok := api.GuestEmulators[e.Guest]
	if !ok {
		return fmt.Errorf("no emulator path for %s", e.Guest)
	}
	if err := Put(root, p, e.Bin, 0o755, t); err != nil {
		return err
	}
	return Put(root, api.GuestEmulateFile, []byte(e.Guest+"\n"), 0o644, t)
}
