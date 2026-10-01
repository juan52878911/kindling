package android

import (
	"context"
	"fmt"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
)

// LA BASE. La misma que hace 71-build-glibc-base.sh con debootstrap
// (SUITE=trixie, PKGS="iptables procps iproute2 dmsetup ca-certificates",
// sin puente): la base Debian fijada de internal/imagen (debian:trixie-slim
// por digest más los .deb de imagen.DebianLock), con el init que monta la capa
// a través de dm-verity. Todo lo que no es de Android está en internal/imagen.

// marca es lo que el constructor android deja con su nombre en la imagen.
var marca = imagen.Marca{Constructor: "android", Dir: "/etc/kindling-android", DM: "android-layer"}

// buildBase escribe la base en dst. table es la tabla de dm-verity de la
// capa (vacía = sin verity).
func (b *builder) buildBase(ctx context.Context, dst, table string) (map[string]any, error) {
	lock, ok := imagen.DebianLock[b.spec.Arch]
	if !ok {
		return nil, fmt.Errorf("no pinned Debian base for %s", b.spec.Arch)
	}
	want := map[string]bool{}
	for _, p := range b.spec.Packages {
		want[p] = true
	}
	var pins []imagen.DebPin
	for _, pin := range lock.Packages {
		if len(want) == 0 || want[pin.Name] {
			pins = append(pins, pin)
		}
	}
	base, err := imagen.PrepareBase(ctx, b.oci, lock, b.spec.Arch, pins, b.debs())
	if err != nil {
		return nil, err
	}
	stats, err := base.Write(dst, b.uuid("base"), table)
	if err != nil {
		return nil, err
	}
	b.logf("base: %d MiB, %d files, %d packages added", stats.Bytes()>>20, stats.Files, len(base.Packages))
	return map[string]any{"debian": base.Image, "packages": base.Packages}, nil
}

func (b *builder) debs() imagen.Debs {
	return imagen.Debs{Cache: b.cache, T: b.t, Marca: marca, Logf: b.logf}
}

func (b *builder) put(root *ext4.Node, p string, data []byte, mode uint32) {
	if err := imagen.Put(root, p, data, mode, b.t); err != nil {
		b.errs = append(b.errs, err)
	}
}

func (b *builder) putFile(root *ext4.Node, p string, n *ext4.Node) {
	if err := imagen.PutNode(root, p, n, b.t); err != nil {
		b.errs = append(b.errs, err)
	}
}
