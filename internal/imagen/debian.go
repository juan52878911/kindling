package imagen

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
	"time"

	"github.com/juan52878911/kindling/internal/deb"
	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/scripts"
)

// LA BASE DEBIAN. La misma que hace 71-build-glibc-base.sh con debootstrap,
// pero armada en Go: una imagen OCI de Debian por digest (debian:trixie-slim,
// que ya trae util-linux: nsenter, unshare, mount, pivot_root), más los .deb
// fijados descomprimidos encima.
//
// Lo que dpkg haría y aquí se hace a mano: el paquete queda apuntado en
// /var/lib/dpkg/status con su lista de ficheros y md5sums (apt lo ve
// instalado en una capa derivada), iptables apunta a iptables-legacy (el
// núcleo del prototipo no trae nf_tables) y ca-certificates deja su
// ca-certificates.crt. Lo que NO se hace: ejecutar los scripts de los
// paquetes (postinst) ni regenerar /etc/ld.so.cache (ld.so busca igual en
// /usr/lib/<triplete>, que es donde van todas las bibliotecas añadidas).

// Debs baja e instala .deb fijados.
type Debs struct {
	Cache    string // directorio de la caché de .deb (verificados por sha256)
	Snapshot string // marca de snapshot.debian.org de respaldo
	T        time.Time
	Marca    Marca
	Logf     func(format string, a ...any)
}

func (d Debs) logf(format string, a ...any) {
	if d.Logf != nil {
		d.Logf(format, a...)
	}
}

// SnapshotURLs son de dónde se puede bajar pin: su URL y, si es de
// deb.debian.org o security.debian.org, la misma ruta en snapshot.
func SnapshotURLs(pinURL, snapshot string) []string {
	urls := []string{pinURL}
	for _, a := range []struct{ from, to string }{
		{"https://deb.debian.org/debian/", "https://snapshot.debian.org/archive/debian/" + snapshot + "/"},
		{"https://security.debian.org/debian-security/", "https://snapshot.debian.org/archive/debian-security/" + snapshot + "/"},
	} {
		if strings.HasPrefix(pinURL, a.from) && snapshot != "" {
			urls = append(urls, a.to+strings.TrimPrefix(pinURL, a.from))
		}
	}
	return urls
}

// Fetch deja el .deb en la caché, verificado.
func (d Debs) Fetch(ctx context.Context, pin DebPin) (string, error) {
	dir := filepath.Join(d.Cache, "debs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, pin.SHA256+".deb")
	// Tres vueltas por las URL: deb.debian.org y snapshot a veces no dan la
	// mano (TLS handshake timeout medido en el laboratorio) y una construcción
	// de 30 paquetes no debería caerse por uno.
	var last error
	for round := 0; round < 3; round++ {
		for _, u := range SnapshotURLs(pin.URL, d.Snapshot) {
			if last = FetchVerified(ctx, u, pin.SHA256, pin.Size, dst); last == nil {
				return dst, nil
			}
			d.logf("%s: %v", path.Base(u), last)
		}
	}
	return "", fmt.Errorf("%s %s: %w", pin.Name, pin.Version, last)
}

// Install descomprime los .deb en root y los apunta en el status de dpkg:
// status es el texto del que se parte (nil = el /var/lib/dpkg/status que ya
// haya en root) y el resultado queda en root/var/lib/dpkg/status. Devuelve los
// flujos con los datos de los paquetes añadidos detrás de streams y la lista
// "nombre=versión".
func (d Debs) Install(ctx context.Context, root *ext4.Node, streams []ext4.Stream, status []byte, pins []DebPin) ([]ext4.Stream, []string, error) {
	statusNode := root.Lookup("/var/lib/dpkg/status")
	if status == nil {
		if statusNode == nil || !statusNode.IsReg() {
			return nil, nil, fmt.Errorf("the Debian image has no /var/lib/dpkg/status")
		}
		text, _ := statusNode.Data.(ext4.Bytes)
		status = text
	}
	st := bytes.NewBuffer(append([]byte{}, status...))
	var names []string
	var errs []error
	put := func(p string, data []byte, mode uint32) {
		if err := Put(root, p, data, mode, d.T); err != nil {
			errs = append(errs, err)
		}
	}
	for _, pin := range pins {
		f, err := d.Fetch(ctx, pin)
		if err != nil {
			return nil, nil, err
		}
		dp, err := deb.Open(f)
		if err != nil {
			return nil, nil, err
		}
		ctrl, err := dp.Control()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", pin.Name, err)
		}
		para, err := deb.ParseParagraph(ctrl["control"])
		if err != nil {
			return nil, nil, fmt.Errorf("%s control: %w", pin.Name, err)
		}
		conff := map[string]bool{}
		for _, l := range strings.Split(string(ctrl["conffiles"]), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "remove-on-upgrade") {
				conff[l] = true
			}
		}
		var list []string
		rc, err := dp.Data()
		if err != nil {
			return nil, nil, err
		}
		err = root.AddTar(rc, ext4.TarOptions{Stream: len(streams), Time: d.T,
			Keep: func(p string, _ int64) bool {
				return conff[p] || strings.HasPrefix(p, "/usr/share/ca-certificates/")
			},
			OnEntry: func(rel string, _ *tar.Header) { list = append(list, rel) }})
		rc.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("%s data: %w", pin.Name, err)
		}
		streams = append(streams, ext4.TarStream(dp.Data))

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
		// Si el paquete ya estaba (una actualización de seguridad de algo que
		// trae la imagen base), su párrafo viejo sale del status: dpkg no
		// admite dos párrafos del mismo paquete y arquitectura.
		if kept := dropStatus(st.Bytes(), para.Get("Package"), arch); len(kept) != st.Len() {
			st = bytes.NewBuffer(kept)
		}
		st.WriteString("\n")
		st.WriteString(para.String())
		lines := []string{"/."}
		for _, l := range list {
			if l != "/" {
				lines = append(lines, l)
			}
		}
		put("/var/lib/dpkg/info/"+info+".list", []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		for k, v := range ctrl {
			if k == "control" {
				continue
			}
			mode := uint32(0o644)
			switch k {
			case "preinst", "postinst", "prerm", "postrm", "config":
				mode = 0o755
			}
			put("/var/lib/dpkg/info/"+info+"."+k, v, mode)
		}
		names = append(names, pin.Name+"="+pin.Version)
	}
	if len(errs) > 0 {
		return nil, nil, errs[0]
	}
	// El status tiene que acabar en una línea en blanco entre párrafos.
	text := bytes.ReplaceAll(st.Bytes(), []byte("\n\n\n"), []byte("\n\n"))
	if statusNode != nil && statusNode.IsReg() {
		statusNode.Data = ext4.Bytes(text)
		statusNode.Size = int64(len(text))
		statusNode.Mtime = d.T
	} else if err := Put(root, "/var/lib/dpkg/status", text, 0o644, d.T); err != nil {
		return nil, nil, err
	}
	return streams, names, nil
}

// dropStatus quita de status los párrafos de pkg con arquitectura arch; el
// resto queda tal cual, byte a byte.
func dropStatus(status []byte, pkg, arch string) []byte {
	needle := []byte("Package: " + pkg + "\n")
	if !bytes.HasPrefix(status, needle) && !bytes.Contains(status, append([]byte("\n"), needle...)) {
		return status
	}
	paras := bytes.Split(status, []byte("\n\n"))
	out := paras[:0]
	for _, p := range paras {
		if !bytes.Contains(p, needle) {
			out = append(out, p)
			continue
		}
		if ps, err := deb.ParseParagraphs(bytes.NewReader(p)); err == nil && len(ps) == 1 &&
			ps[0].Get("Package") == pkg && ps[0].Get("Architecture") == arch {
			continue
		}
		out = append(out, p)
	}
	return bytes.Join(out, []byte("\n\n"))
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

// Ajustar hace en root lo que dejarían los postinst que importan y adelgaza
// como 71-build-glibc-base.sh: iptables -> legacy, el ca-certificates.crt y
// fuera documentación, man y locales. Con base, además vacía /var/log y las
// listas de apt (en una capa no hay nada de eso que vaciar).
func (d Debs) Ajustar(root *ext4.Node, base bool) error {
	var errs []error
	put := func(p string, data []byte) {
		if err := Put(root, p, data, 0o644, d.T); err != nil {
			errs = append(errs, err)
		}
	}
	link := func(p, target string) {
		if err := Link(root, p, target, d.T); err != nil {
			errs = append(errs, err)
		}
	}
	// iptables -> legacy (lo que haría update-alternatives --set). El núcleo
	// no trae nf_tables; kling-phoned y android-launch.sh ya buscan
	// iptables-legacy primero, pero así también `iptables` a secas funciona.
	if root.Lookup("/usr/sbin/iptables-legacy") != nil {
		for _, t := range []string{"iptables", "iptables-restore", "iptables-save", "ip6tables", "ip6tables-restore", "ip6tables-save"} {
			link("/etc/alternatives/"+t, "/usr/sbin/"+strings.Replace(t, "tables", "tables-legacy", 1))
			link("/usr/sbin/"+t, "/etc/alternatives/"+t)
		}
	}
	// ca-certificates: el conf y el paquete de certificados que deja su postinst.
	if dir := root.Lookup("/usr/share/ca-certificates/mozilla"); dir != nil && dir.IsDir() {
		var conf, bundle bytes.Buffer
		conf.WriteString("# Generado por el " + d.Marca.quien() + " (lo que deja el postinst de ca-certificates).\n")
		for _, name := range dir.Children() {
			n := dir.Child(name)
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
		put("/etc/ca-certificates.conf", conf.Bytes())
		put("/etc/ssl/certs/ca-certificates.crt", bundle.Bytes())
	}
	// Adelgazar, como 71-build-glibc-base.sh.
	for _, p := range []string{"/usr/share/doc", "/usr/share/man", "/usr/share/info", "/usr/share/locale",
		"/usr/share/i18n", "/var/cache/apt"} {
		root.Remove(p)
	}
	if base {
		for _, dir := range []string{"/var/log", "/var/lib/apt/lists"} {
			if n := root.Lookup(dir); n != nil && n.IsDir() {
				for _, c := range n.Children() {
					if !(dir == "/var/lib/apt/lists" && c == "partial") {
						n.RemoveChild(c)
					}
				}
			}
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// Base es una base Debian armada en memoria, a falta del init.
type Base struct {
	Root     *ext4.Node
	Streams  []ext4.Stream
	Image    string   // la imagen OCI de la que salió (repo@digest)
	Packages []string // los .deb añadidos, "nombre=versión"
	debs     Debs
}

// PrepareBase arma la base: la imagen de lock (para arch) con los .deb pins
// encima, ajustada (Ajustar). Lo que falta para escribirla lo pone Write.
func PrepareBase(ctx context.Context, c *oci.Client, lock DebianBase, arch string, pins []DebPin, d Debs) (*Base, error) {
	img, err := c.Pull(ctx, lock.Image, lock.Manifest, arch)
	if err != nil {
		return nil, fmt.Errorf("debian base image: %w", err)
	}
	d.logf("base: %s (%d layers)", img.Ref, len(img.Layers))
	root := ext4.NewDir(0o755, 0, 0, d.T)
	var streams []ext4.Stream
	for i, l := range img.Layers {
		rc, err := oci.OpenLayer(l)
		if err != nil {
			return nil, err
		}
		err = root.AddTar(rc, ext4.TarOptions{Stream: len(streams), Whiteouts: true, Time: d.T,
			Keep: func(p string, _ int64) bool { return p == "/var/lib/dpkg/status" }})
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("debian layer %d: %w", i, err)
		}
		l := l
		streams = append(streams, ext4.TarStream(func() (io.ReadCloser, error) { return oci.OpenLayer(l) }))
	}
	if d.Snapshot == "" {
		d.Snapshot = lock.Snapshot
	}
	streams, names, err := d.Install(ctx, root, streams, nil, pins)
	if err != nil {
		return nil, err
	}
	if err := d.Ajustar(root, true); err != nil {
		return nil, err
	}
	return &Base{Root: root, Streams: streams, Image: img.Ref, Packages: names, debs: d}, nil
}

// Status es el /var/lib/dpkg/status de la base (del que parte el de una capa
// que instala más paquetes).
func (b *Base) Status() []byte { return StatusOf(b.Root) }

// Write pone el resolv.conf, el init (minimal-init.sh; con table, el que
// monta la capa a través de dm-verity, y la tabla) y los puntos de montaje, y
// escribe la base en dst.
func (b *Base) Write(dst string, uuid [16]byte, table string) (ext4.Stats, error) {
	t, m := b.debs.T, b.debs.Marca
	if err := Put(b.Root, "/etc/resolv.conf", []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644, t); err != nil {
		return ext4.Stats{}, err
	}
	init := scripts.MinimalInit
	if table != "" {
		var err error
		if init, err = VerityInit(init, m); err != nil {
			return ext4.Stats{}, err
		}
		if err := Put(b.Root, m.VerityConf(), VerityFile(m, table), 0o644, t); err != nil {
			return ext4.Stats{}, err
		}
	}
	if err := Put(b.Root, "/usr/sbin/overlay-init", []byte(init), 0o755, t); err != nil {
		return ext4.Stats{}, err
	}
	for _, d := range []string{"/overlay", "/rom", "/run"} {
		if _, err := b.Root.MkdirAll(d, 0o755, 0, 0, t); err != nil {
			return ext4.Stats{}, err
		}
	}
	f, err := os.Create(dst)
	if err != nil {
		return ext4.Stats{}, err
	}
	defer f.Close()
	// 32 MiB de holgura, como 71-build-glibc-base.sh: cabe un recambio del
	// puente o un fichero puesto con PUT /images/{name}/files.
	stats, err := ext4.Write(f, b.Root, b.Streams, ext4.Options{
		Time: t, UUID: uuid, LostFound: true, ZeroHoles: true,
		SlackBlocks: 32 << 20 / ext4.BlockSize, SlackInodes: 1024,
	})
	if err != nil {
		return ext4.Stats{}, fmt.Errorf("writing the base: %w", err)
	}
	return stats, f.Close()
}
