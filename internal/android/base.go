package android

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/deb"
	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/scripts"
)

// LA BASE. La misma que hace 71-build-glibc-base.sh con debootstrap
// (SUITE=trixie, PKGS="iproute2 ca-certificates util-linux procps iptables
// dmsetup", sin puente), pero armada en Go: debian:trixie-slim por digest,
// que ya trae util-linux (nsenter, unshare, mount, pivot_root), más los .deb
// fijados en debian_lock.go descomprimidos encima.
//
// Lo que dpkg haría y aquí se hace a mano: el paquete queda apuntado en
// /var/lib/dpkg/status con su lista de ficheros y md5sums (apt lo ve
// instalado en una capa derivada), iptables apunta a iptables-legacy (el
// núcleo del prototipo no trae nf_tables) y ca-certificates deja su
// ca-certificates.crt. Lo que NO se hace: ejecutar los scripts de los
// paquetes (postinst) ni regenerar /etc/ld.so.cache (ld.so busca igual en
// /usr/lib/<triplete>, que es donde van todas las bibliotecas añadidas).

// buildBase escribe la base en dst. table es la tabla de dm-verity de la
// capa (vacía = sin verity).
func (b *builder) buildBase(ctx context.Context, dst, table string) (map[string]any, error) {
	lock, ok := debianLock[b.spec.Arch]
	if !ok {
		return nil, fmt.Errorf("no pinned Debian base for %s", b.spec.Arch)
	}
	img, err := b.oci.Pull(ctx, lock.Image, lock.Manifest, b.spec.Arch)
	if err != nil {
		return nil, fmt.Errorf("debian base image: %w", err)
	}
	b.logf("base: %s (%d layers)", img.Ref, len(img.Layers))
	root := ext4.NewDir(0o755, 0, 0, b.t)
	var streams []ext4.Stream
	for i, l := range img.Layers {
		rc, err := oci.OpenLayer(l)
		if err != nil {
			return nil, err
		}
		err = root.AddTar(rc, ext4.TarOptions{Stream: len(streams), Whiteouts: true, Time: b.t,
			Keep: func(p string, _ int64) bool { return p == "/var/lib/dpkg/status" }})
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("debian layer %d: %w", i, err)
		}
		l := l
		streams = append(streams, ext4.TarStream(func() (io.ReadCloser, error) { return oci.OpenLayer(l) }))
	}

	status := root.Lookup("/var/lib/dpkg/status")
	if status == nil || !status.IsReg() {
		return nil, fmt.Errorf("the Debian image has no /var/lib/dpkg/status")
	}
	statusText, _ := status.Data.(ext4.Bytes)
	st := bytes.NewBuffer(append([]byte{}, statusText...))

	want := map[string]bool{}
	for _, p := range b.spec.Packages {
		want[p] = true
	}
	var names []string
	for _, pin := range lock.Packages {
		if len(want) > 0 && !want[pin.Name] {
			continue
		}
		f, err := b.fetchDeb(ctx, pin, lock.Snapshot)
		if err != nil {
			return nil, err
		}
		d, err := deb.Open(f)
		if err != nil {
			return nil, err
		}
		ctrl, err := d.Control()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pin.Name, err)
		}
		para, err := deb.ParseParagraph(ctrl["control"])
		if err != nil {
			return nil, fmt.Errorf("%s control: %w", pin.Name, err)
		}
		conff := map[string]bool{}
		for _, l := range strings.Split(string(ctrl["conffiles"]), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "remove-on-upgrade") {
				conff[l] = true
			}
		}
		var list []string
		rc, err := d.Data()
		if err != nil {
			return nil, err
		}
		err = root.AddTar(rc, ext4.TarOptions{Stream: len(streams), Time: b.t,
			Keep: func(p string, _ int64) bool {
				return conff[p] || strings.HasPrefix(p, "/usr/share/ca-certificates/")
			},
			OnEntry: func(rel string, _ *tar.Header) { list = append(list, rel) }})
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s data: %w", pin.Name, err)
		}
		streams = append(streams, ext4.TarStream(d.Data))

		// dpkg: status, lista de ficheros, md5sums y scripts.
		arch := para.Get("Architecture")
		info := para.Get("Package")
		if para.Get("Multi-Arch") == "same" {
			info += ":" + arch
		}
		para.Keys = append([]string{"Package", "Status"}, without(para.Keys, "Package")...)
		para.Values["Status"] = "install ok installed"
		if len(conff) > 0 {
			var cl []string
			for c := range conff {
				sum := "newconffile"
				if n := root.Lookup(c); n != nil {
					if data, ok := n.Data.(ext4.Bytes); ok {
						h := md5.Sum(data)
						sum = hex.EncodeToString(h[:])
					}
				}
				cl = append(cl, " "+c+" "+sum)
			}
			sort.Strings(cl)
			para.Keys = append(para.Keys, "Conffiles")
			para.Values["Conffiles"] = "\n" + strings.Join(cl, "\n")
		}
		st.WriteString("\n")
		st.WriteString(para.String())
		lines := []string{"/."}
		for _, l := range list {
			if l != "/" {
				lines = append(lines, l)
			}
		}
		b.put(root, "/var/lib/dpkg/info/"+info+".list", []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		for k, v := range ctrl {
			if k == "control" {
				continue
			}
			mode := uint32(0o644)
			switch k {
			case "preinst", "postinst", "prerm", "postrm", "config":
				mode = 0o755
			}
			b.put(root, "/var/lib/dpkg/info/"+info+"."+k, v, mode)
		}
		names = append(names, pin.Name+"="+pin.Version)
	}
	// El status tiene que acabar en una línea en blanco entre párrafos.
	status.Data = ext4.Bytes(bytes.ReplaceAll(st.Bytes(), []byte("\n\n\n"), []byte("\n\n")))
	status.Size = int64(len(status.Data.(ext4.Bytes)))
	status.Mtime = b.t

	// iptables -> legacy (lo que haría update-alternatives --set). El núcleo
	// no trae nf_tables; kling-phoned y android-launch.sh ya buscan
	// iptables-legacy primero, pero así también `iptables` a secas funciona.
	if root.Lookup("/usr/sbin/iptables-legacy") != nil {
		for _, t := range []string{"iptables", "iptables-restore", "iptables-save", "ip6tables", "ip6tables-restore", "ip6tables-save"} {
			b.link(root, "/etc/alternatives/"+t, "/usr/sbin/"+strings.Replace(t, "tables", "tables-legacy", 1))
			b.link(root, "/usr/sbin/"+t, "/etc/alternatives/"+t)
		}
	}
	// ca-certificates: el conf y el paquete de certificados que deja su postinst.
	if d := root.Lookup("/usr/share/ca-certificates/mozilla"); d != nil && d.IsDir() {
		var conf, bundle bytes.Buffer
		conf.WriteString("# Generado por el constructor android de kindling (lo que deja el postinst de ca-certificates).\n")
		for _, name := range d.Children() {
			n := d.Child(name)
			data, ok := n.Data.(ext4.Bytes)
			if !ok || !strings.HasSuffix(name, ".crt") {
				continue
			}
			conf.WriteString("mozilla/" + name + "\n")
			bundle.Write(data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				bundle.WriteByte('\n')
			}
		}
		b.put(root, "/etc/ca-certificates.conf", conf.Bytes(), 0o644)
		b.put(root, "/etc/ssl/certs/ca-certificates.crt", bundle.Bytes(), 0o644)
	}

	// Adelgazar, como 71-build-glibc-base.sh.
	for _, p := range []string{"/usr/share/doc", "/usr/share/man", "/usr/share/info", "/usr/share/locale",
		"/usr/share/i18n", "/var/cache/apt"} {
		root.Remove(p)
	}
	for _, d := range []string{"/var/log", "/var/lib/apt/lists"} {
		if n := root.Lookup(d); n != nil && n.IsDir() {
			for _, c := range n.Children() {
				if !(d == "/var/lib/apt/lists" && c == "partial") {
					n.RemoveChild(c)
				}
			}
		}
	}
	b.put(root, "/etc/resolv.conf", []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644)
	init := scripts.MinimalInit
	if table != "" {
		init, err = verityInit(init)
		if err != nil {
			return nil, err
		}
		b.put(root, "/etc/kindling-android/layer.verity", []byte(
			"# dm-verity de la capa (constructor android de kindling). Lo lee /sbin/overlay-init.\n"+table+"\n"), 0o644)
	}
	b.put(root, "/usr/sbin/overlay-init", []byte(init), 0o755)
	for _, d := range []string{"/overlay", "/rom", "/run"} {
		if _, err := root.MkdirAll(d, 0o755, 0, 0, b.t); err != nil {
			return nil, err
		}
	}

	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// 32 MiB de holgura, como 71-build-glibc-base.sh: cabe un recambio del
	// puente o un fichero puesto con PUT /images/{name}/files.
	stats, err := ext4.Write(f, root, streams, ext4.Options{
		Time: b.t, UUID: b.uuid("base"), LostFound: true, ZeroHoles: true,
		SlackBlocks: 32 << 20 / ext4.BlockSize, SlackInodes: 1024,
	})
	if err != nil {
		return nil, fmt.Errorf("writing the base: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	b.logf("base: %d MiB, %d files, %d packages added", stats.Bytes()>>20, stats.Files, len(names))
	return map[string]any{"debian": img.Ref, "packages": names}, nil
}

func without(keys []string, k string) []string {
	var out []string
	for _, x := range keys {
		if !strings.EqualFold(x, k) {
			out = append(out, x)
		}
	}
	return out
}

// verityAnchor es la línea de minimal-init.sh que monta la capa.
const verityAnchor = `  mount -t ext4 -o ro "$LAYER_DEV" /overlay/svc`

// verityBlock es lo que verity.sh mete antes: la capa por /dev/mapper, y si
// dmsetup falla el init se para (montarla sin verificar sería el fallo de
// siempre).
const verityBlock = `  # kindling-android: dm-verity (constructor android de kindling). La capa
  # se lee a través de dm-verity: un bloque que no cuadra con su hash da EIO
  # (y el kernel lo relee) en vez de entrar en la caché como bueno.
  if [ -f /etc/kindling-android/layer.verity ]; then
    tabla="$(grep -v '^#' /etc/kindling-android/layer.verity | sed "s#@DEV@#$LAYER_DEV#g")"
    if ! DM_DISABLE_UDEV=1 dmsetup create android-layer --readonly --table "$tabla"; then
      echo "kindling-android: dm-verity on $LAYER_DEV failed; refusing to mount the layer unverified" >&2
      exit 1
    fi
    LAYER_DEV=/dev/mapper/android-layer
  fi
`

func verityInit(init string) (string, error) {
	lines := strings.Split(init, "\n")
	for i, l := range lines {
		if l == verityAnchor {
			out := append([]string{}, lines[:i]...)
			out = append(out, strings.Split(strings.TrimSuffix(verityBlock, "\n"), "\n")...)
			out = append(out, lines[i:]...)
			return strings.Join(out, "\n"), nil
		}
	}
	return "", fmt.Errorf("minimal-init.sh changed: can't find where the layer is mounted (%q)", verityAnchor)
}

// fetchDeb deja el .deb en la caché, verificado.
func (b *builder) fetchDeb(ctx context.Context, pin debPin, snapshot string) (string, error) {
	dir := filepath.Join(b.cache, "debs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, pin.SHA256+".deb")
	urls := []string{pin.URL}
	for _, a := range []struct{ from, to string }{
		{"https://deb.debian.org/debian/", "https://snapshot.debian.org/archive/debian/" + snapshot + "/"},
		{"https://security.debian.org/debian-security/", "https://snapshot.debian.org/archive/debian-security/" + snapshot + "/"},
	} {
		if strings.HasPrefix(pin.URL, a.from) && snapshot != "" {
			urls = append(urls, a.to+strings.TrimPrefix(pin.URL, a.from))
		}
	}
	var last error
	for _, u := range urls {
		if last = fetchVerified(ctx, u, pin.SHA256, pin.Size, dst); last == nil {
			return dst, nil
		}
		b.logf("%s: %v", path.Base(u), last)
	}
	return "", fmt.Errorf("%s %s: %w", pin.Name, pin.Version, last)
}

func (b *builder) put(root *ext4.Node, p string, data []byte, mode uint32) {
	n := &ext4.Node{Mode: ext4.ModeReg | mode, Mtime: b.t, Size: int64(len(data)), Data: ext4.Bytes(data)}
	if err := root.Put(p, n, b.t); err != nil {
		b.errs = append(b.errs, fmt.Errorf("%s: %w", p, err))
		return
	}
	touchParent(root, p, b.t)
}

func (b *builder) link(root *ext4.Node, p, target string) {
	if err := root.Put(p, &ext4.Node{Mode: ext4.ModeLink | 0o777, Target: target, Mtime: b.t}, b.t); err != nil {
		b.errs = append(b.errs, fmt.Errorf("%s: %w", p, err))
	}
}
