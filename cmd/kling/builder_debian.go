package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
)

// CONSTRUCTOR "debian": una imagen Debian (trixie) fijada, con los paquetes
// de la receta, armada entera en Go (internal/imagen): sin root, loop,
// chroot, debootstrap ni apt. Corre igual en el host Linux del daemon que en
// el daemon de macOS (kling-vz) o en un host sin loop.
//
// Salen dos ficheros, como con el de android:
//   - la base <base>.ext4: debian:trixie-slim por digest + los .deb fijados
//     de la base de kindling (imagen.DebianLock: iproute2, iptables, procps,
//     dmsetup, ca-certificates) + el init (minimal-init.sh);
//   - la capa <nombre>.layer.ext4: en /upper los paquetes de la receta, su
//     status de dpkg, el agente de invitado y el /entrypoint.
//
// Los paquetes se resuelven contra los índices de snapshot.debian.org del
// mismo instante en que se fijó la base, y cada .deb se baja y se comprueba
// por sha256. Lo resuelto (el lockfile) queda en la receta (built.lock); con
// "lock" en el spec no se resuelve nada y se baja exactamente eso.
//
// Con "verity": true la capa va detrás de dm-verity: el árbol de hashes y el
// FEC se pegan a la capa y la tabla (con la raíz) va en la base y en la
// receta; el init no monta la capa si dmsetup no la acepta.

// DebianBaseBuilder es el constructor que figura en la receta de las bases
// que escribe este constructor: solo se sobrescribe una base que lo lleve.
const DebianBaseBuilder = "debian-base"

var marcaDebian = imagen.Marca{Constructor: "debian", Dir: "/etc/kindling", DM: "kindling-layer"}

// DebianSpec es el spec del constructor "debian".
type DebianSpec struct {
	// Arch es amd64 o arm64 (por defecto, la del host).
	Arch string `json:"arch,omitempty"`
	// Packages son paquetes de Debian trixie que se añaden (con sus
	// dependencias, sin Recommends).
	Packages []string `json:"packages,omitempty"`
	// Lock fija los .deb exactos (el built.lock de una construcción
	// anterior): no se resuelve nada.
	Lock []imagen.DebPin `json:"lock,omitempty"`
	// Env son variables KEY=VALUE que el /entrypoint carga de /etc/kling/env (0600).
	Env []string `json:"env,omitempty"`
	// Service es un ejecutable de la imagen que el /entrypoint arranca (y
	// relanza) antes de ceder el PID 1 al agente. Vacío: solo el agente.
	Service string `json:"service,omitempty"`
	// Verity pone la capa detrás de dm-verity con FECRoots raíces de
	// Reed-Solomon (por defecto 2; 0 = sin FEC).
	Verity   bool `json:"verity,omitempty"`
	FECRoots *int `json:"fec_roots,omitempty"`
	// BaseName es cómo se llama la base (por defecto "<nombre>-base", o la
	// base de la petición). Es de esta capa: con verity lleva su tabla.
	BaseName string `json:"base_name,omitempty"`
}

func (s DebianSpec) fecRoots() int {
	if s.FECRoots == nil {
		return 2
	}
	return *s.FECRoots
}

func validateDebian(req api.BuildImageRequest, s DebianSpec) error {
	if !reBuildName.MatchString(req.Name) {
		return fmt.Errorf("invalid image name %q", req.Name)
	}
	if _, ok := imagen.DebianLock[s.Arch]; !ok {
		return fmt.Errorf("arch must be amd64 or arm64, not %q", s.Arch)
	}
	if s.BaseName != "" && !reBuildName.MatchString(s.BaseName) {
		return fmt.Errorf("invalid base_name %q", s.BaseName)
	}
	if len(s.Packages) > 64 {
		return fmt.Errorf("too many packages (%d, max 64)", len(s.Packages))
	}
	for _, p := range s.Packages {
		if !reBuildPkg.MatchString(p) || strings.ToLower(p) != p {
			return fmt.Errorf("invalid package name %q", p)
		}
	}
	if len(s.Lock) > 1024 {
		return fmt.Errorf("lock too long (%d entries, max 1024)", len(s.Lock))
	}
	locked := map[string]bool{}
	for _, p := range s.Lock {
		if err := imagen.CheckPin(p); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		locked[p.Name] = true
	}
	if len(s.Lock) > 0 {
		for _, p := range s.Packages {
			if !locked[p] {
				return fmt.Errorf("package %q is not in the lock", p)
			}
		}
	}
	for _, kv := range s.Env {
		if !reBuildEnv.MatchString(kv) {
			return fmt.Errorf("invalid env entry %q: use KEY=value, one line", kv)
		}
	}
	if s.Service != "" && (!path.IsAbs(s.Service) || path.Clean(s.Service) != s.Service || strings.ContainsAny(s.Service, "\x00\r\n")) {
		return fmt.Errorf("service must be a clean absolute path inside the image")
	}
	if r := s.fecRoots(); r != 0 && (r < 2 || r > 24) {
		return fmt.Errorf("fec_roots must be 0 or 2..24")
	}
	return nil
}

func builderDebian(dir string) error { return buildDebian(context.Background(), dir, os.Stdout) }

func buildDebian(ctx context.Context, dir string, log io.Writer) error {
	t0 := time.Now()
	logf := func(format string, a ...any) { fmt.Fprintf(log, format+"\n", a...) }
	raw, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return err
	}
	var req api.BuildImageRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("request.json: %w", err)
	}
	var spec DebianSpec
	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &spec); err != nil {
			return fmt.Errorf("spec: %w", err)
		}
	}
	if spec.Arch == "" {
		spec.Arch = runtime.GOARCH
	}
	if err := validateDebian(req, spec); err != nil {
		return err
	}
	baseName := spec.BaseName
	if baseName == "" {
		baseName = req.Base
	}
	if baseName == "" {
		baseName = req.Name + "-base"
	}
	if !reBuildName.MatchString(baseName) || baseName == req.Name {
		return fmt.Errorf("invalid base name %q", baseName)
	}
	root := envOr("KLING_ROOT", "/var/lib/kindling")
	lib := envOr("KLING_LIB_DIR", libPorDefecto(root))
	agent := envOr("KLING_GUEST_AGENT", filepath.Join(lib, "kling-guest"))
	if spec.Arch != runtime.GOARCH {
		// El agente instalado es el del host; para otra arquitectura hay que
		// dar el suyo.
		if a := os.Getenv("KLING_GUEST_AGENT_" + spec.Arch); a != "" {
			agent = a
		}
	}
	agentSum, err := imagen.SHA256File(agent)
	if err != nil {
		return fmt.Errorf("guest agent: %w (%s)", err, pistaAgente())
	}
	t := time.Now().UTC().Truncate(time.Second)
	if e := os.Getenv("SOURCE_DATE_EPOCH"); e != "" {
		if s, err := strconv.ParseInt(e, 10, 64); err == nil {
			t = time.Unix(s, 0).UTC()
		}
	}
	cache := filepath.Join(root, "cache")
	images := filepath.Join(root, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		return err
	}
	if err := imagen.CheckBaseOwner(images, baseName, DebianBaseBuilder, "debian"); err != nil {
		return err
	}

	lock := imagen.DebianLock[spec.Arch]
	d := imagen.Debs{Cache: cache, Snapshot: lock.Snapshot, T: t, Marca: marcaDebian, Logf: logf}
	base, err := imagen.PrepareBase(ctx, &oci.Client{Cache: filepath.Join(cache, "oci"), Log: log}, lock, spec.Arch, lock.Packages, d)
	if err != nil {
		return err
	}
	status := base.Status()
	pins := spec.Lock
	if len(pins) == 0 && len(spec.Packages) > 0 {
		t1 := time.Now()
		if pins, err = d.Resolve(ctx, spec.Arch, status, spec.Packages); err != nil {
			return fmt.Errorf("resolving %s: %w", strings.Join(spec.Packages, " "), err)
		}
		logf("resolved %d package(s) against snapshot %s in %.1f s", len(pins), lock.Snapshot, time.Since(t1).Seconds())
	}

	// La identidad de la construcción: con las mismas entradas y la misma
	// hora (SOURCE_DATE_EPOCH) sale la misma imagen, bit a bit, con la misma
	// raíz de verity. El lock entra por lo que fija (pins), no por cómo
	// llegó: resolver o dar el lock de esa resolución da la misma imagen.
	h := sha256.New()
	specID := spec
	specID.Lock = nil
	sb, _ := json.Marshal(specID)
	fmt.Fprintf(h, "kindling-debian-v1\x00%s\x00%s\x00%s\x00%s\x00%d\x00%+v\x00%+v\x00", req.Name, baseName, sb, agentSum, t.Unix(), lock, pins)
	var id [32]byte
	copy(id[:], h.Sum(nil))
	const tag = "kindling-debian"

	// La capa.
	layer := ext4.NewDir(0o755, 0, 0, t)
	upper, err := layer.MkdirAll("/upper", 0o755, 0, 0, t)
	if err != nil {
		return err
	}
	var streams []ext4.Stream
	var added []string
	if len(pins) > 0 {
		if streams, added, err = d.Install(ctx, upper, nil, status, pins); err != nil {
			return err
		}
		if err := d.Ajustar(upper, false); err != nil {
			return err
		}
	}
	if err := imagen.PutAgent(upper, agent, spec.Arch, t); err != nil {
		return err
	}
	for _, f := range []struct {
		p    string
		data string
		mode uint32
	}{
		{"/entrypoint", imagen.Entrypoint(marcaDebian, spec.Env, spec.Service), 0o755},
		{"/etc/resolv.conf", "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", 0o644},
		{"/etc/kindling/IMAGE.txt", fmt.Sprintf("kindling_builder=debian\ndebian=%s\nsnapshot=%s\npackages=%s\nbuilt_at=%s\n",
			base.Image, lock.Snapshot, strings.Join(added, " "), t.Format("2006-01-02T15:04:05Z")), 0o644},
	} {
		if err := imagen.Put(upper, f.p, []byte(f.data), f.mode, t); err != nil {
			return err
		}
	}
	if env := imagen.EnvFile(spec.Env); env != "" {
		// 0600 de root: el entrypoint es 0755 y lo leería cualquier proceso.
		if err := imagen.Put(upper, imagen.EnvPath, []byte(env), 0o600, t); err != nil {
			return err
		}
	}
	if spec.Service != "" {
		n, _ := upper.Resolve(spec.Service)
		if n == nil {
			n, _ = base.Root.Resolve(spec.Service)
		}
		if n == nil || !n.IsReg() || n.Mode&0o111 == 0 {
			return fmt.Errorf("service %s is not an executable file in the image", spec.Service)
		}
	}

	layerTmp := filepath.Join(images, "."+req.Name+".layer.ext4.tmp")
	baseTmp := filepath.Join(images, "."+baseName+".ext4.tmp")
	defer os.Remove(layerTmp)
	defer os.Remove(baseTmp)
	lstats, err := writeExt4(layerTmp, layer, streams, ext4.Options{
		Time: t, UUID: imagen.UUID(tag, "layer", id), LostFound: true, ZeroHoles: true,
		SlackBlocks: 16 << 20 / ext4.BlockSize, SlackInodes: 256,
	})
	if err != nil {
		return fmt.Errorf("writing the layer: %w", err)
	}
	logf("layer: %d MiB, %d files, %d package(s) added", lstats.Bytes()>>20, lstats.Files, len(added))

	built := map[string]any{"arch": spec.Arch, "base": baseName, "debian": base.Image, "snapshot": lock.Snapshot,
		"base_packages": base.Packages, "packages": added, "lock": pins}
	table := ""
	if spec.Verity {
		t1 := time.Now()
		res, err := imagen.Verity(layerTmp, uint64(lstats.Blocks), tag, id, spec.fecRoots())
		if err != nil {
			return err
		}
		table = res.Table("@DEV@")
		logf("dm-verity: %d data blocks, %d hash blocks, %d FEC blocks (%d roots), root %x (%.1f s)",
			res.DataBlocks, res.HashBlocks, res.FECBlocks, res.FECRoots, res.RootHash, time.Since(t1).Seconds())
		imagen.VerityInfo(built, res, table)
	}
	bstats, err := base.Write(baseTmp, imagen.UUID(tag, "base", id), table)
	if err != nil {
		return err
	}
	logf("base: %d MiB, %d files, %d package(s)", bstats.Bytes()>>20, bstats.Files, len(base.Packages))

	// La base antes que la capa: una capa nueva sobre la base vieja no
	// arrancaría con verity (la tabla no cuadra).
	for _, p := range []string{baseTmp, layerTmp} {
		if err := os.Chmod(p, 0o644); err != nil {
			return err
		}
	}
	if err := os.Rename(baseTmp, filepath.Join(images, baseName+".ext4")); err != nil {
		return err
	}
	if err := os.Rename(layerTmp, filepath.Join(images, req.Name+".layer.ext4")); err != nil {
		return err
	}
	bs, _ := json.Marshal(map[string]any{"for_layer": req.Name, "arch": spec.Arch, "debian": base.Image, "packages": base.Packages})
	baseRec := api.ImageRecipe{Name: baseName, Cmd: []string{}, BuiltAt: t, Builder: DebianBaseBuilder, Spec: bs}
	rb, _ := json.MarshalIndent(baseRec, "", "  ")
	if err := os.WriteFile(filepath.Join(images, baseName+".recipe.json"), append(rb, '\n'), 0o644); err != nil {
		return err
	}
	bj, _ := json.Marshal(built)
	hb, _ := json.MarshalIndent(api.BuildRecipeHints{Base: baseName, Built: bj}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "recipe.json"), append(hb, '\n'), 0o644); err != nil {
		return err
	}
	logf("image %s ready: layer%s over base %s, in %.1f s", req.Name, map[bool]string{true: " + verity"}[spec.Verity], baseName, time.Since(t0).Seconds())
	return nil
}

func writeExt4(dst string, root *ext4.Node, streams []ext4.Stream, opt ext4.Options) (ext4.Stats, error) {
	f, err := os.Create(dst)
	if err != nil {
		return ext4.Stats{}, err
	}
	defer f.Close()
	stats, err := ext4.Write(f, root, streams, opt)
	if err != nil {
		return ext4.Stats{}, err
	}
	return stats, f.Close()
}
