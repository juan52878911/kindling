package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/lazyre"
	"github.com/juan52878911/kindling/scripts"
)

// CONSTRUCTOR "oci": una imagen de Docker/OCI tal cual, como base propia.
//
// La imagen no va encima de la base de kindling (Alpine): se aplana entera
// en un ext4, con sus whiteouts, y es su propia base. Así no se mezclan dos
// /lib (glibc y musl) ni dos /etc. Lo único que se añade es lo de kindling:
//
//   - /sbin/overlay-init (minimal-init.sh, con el contrato de runtime que
//     Docker da por hecho: /dev/fd, /dev/shm, /etc/hosts, hostname);
//   - el agente de invitado, /usr/local/bin/kling-guest, y el /entrypoint
//     que le cede el PID 1;
//   - en /etc/kling/env (0600) el Env de la imagen más el del spec;
//   - en /etc/kindling/service.json el ENTRYPOINT+CMD con el USER, el
//     WORKDIR y la STOPSIGNAL de la imagen: lo arranca y vigila el agente
//     (pkg/guest/service.go), después de montar los volúmenes;
//   - en /etc/kindling/ready la sonda de "listo": el HEALTHCHECK de la
//     imagen o, sin él, que acepte conexiones el primer puerto TCP de EXPOSE;
//   - en /etc/kindling/oci.json la referencia, el digest y la configuración.
//
// Todo en Go (internal/oci, internal/ext4): sin root, loop, chroot ni
// Docker, también en el daemon de macOS. La etiqueta se resuelve a digest una
// vez; la receta lleva el digest, y con él se reconstruye exactamente lo
// mismo. Cada capa se comprueba por sha256 y queda en la caché por hash:
// reimportar el mismo digest no baja nada.
//
// Límites: el init es un script de sh que necesita sh, mount, pivot_root,
// mkdir, ln, cat y grep en la imagen (cualquier Alpine o Debian los trae; una
// "distroless" no, y se rechaza al construir). Sin verity: la imagen es la
// raíz, no una capa.

// OCISpec es el spec del constructor "oci".
type OCISpec struct {
	// Ref es la imagen: "postgres:17-alpine", "ghcr.io/o/r:tag",
	// "repo@sha256:...". Con etiqueta, se resuelve a digest al construir.
	Ref string `json:"ref"`
	// Digest fija la imagen (el del índice o el del manifiesto). Si Ref
	// trae uno, tienen que cuadrar.
	Digest string `json:"digest,omitempty"`
	// Arch es amd64 o arm64 (por defecto, la del host).
	Arch string `json:"arch,omitempty"`
	// Env son variables KEY=VALUE que se suman a las de la imagen (ganan
	// éstas). Van en /etc/kling/env (0600), dentro de la imagen: no son sitio
	// para secretos de verdad.
	Env []string `json:"env,omitempty"`
	// Entrypoint y Cmd sustituyen a los de la imagen, como en docker run.
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	// User sustituye al USER de la imagen ("uid[:gid]" o "nombre[:grupo]").
	User string `json:"user,omitempty"`
	// MaxMB es el tope de lo que se baja (las capas comprimidas). Por
	// defecto 4096; el aplanado no puede pasar de 8 veces eso.
	MaxMB int `json:"max_mb,omitempty"`
}

const (
	ociDefaultMaxMB = 4096
	ociMaxFiles     = 2_000_000
)

// ociInitTools son lo que minimal-init.sh necesita de la imagen.
var ociInitTools = []string{"sh", "mount", "pivot_root", "mkdir", "ln", "cat", "grep"}

// reOCIUser es un USER de Docker: uid o nombre, con grupo opcional.
var reOCIUser = lazyre.New(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,31})?$`)

func validateOCI(req api.BuildImageRequest, s OCISpec) (oci.ImageRef, error) {
	if !reBuildName.MatchString(req.Name) {
		return oci.ImageRef{}, fmt.Errorf("invalid image name %q", req.Name)
	}
	if req.Base != "" {
		return oci.ImageRef{}, fmt.Errorf("the oci builder makes its own base; don't give one")
	}
	ref, err := oci.ParseImageRef(s.Ref)
	if err != nil {
		return ref, err
	}
	if s.Digest != "" {
		if ref.Digest != "" && ref.Digest != s.Digest {
			return ref, fmt.Errorf("ref has digest %s but the spec pins %s", ref.Digest, s.Digest)
		}
		probe, err := oci.ParseImageRef(ref.Name() + "@" + s.Digest)
		if err != nil {
			return ref, err
		}
		ref.Digest = probe.Digest
	}
	if _, ok := imagen.ElfMachine[s.Arch]; !ok {
		return ref, fmt.Errorf("arch must be amd64 or arm64, not %q", s.Arch)
	}
	if len(s.Env) > 256 {
		return ref, fmt.Errorf("too many env entries (%d, max 256)", len(s.Env))
	}
	for _, kv := range s.Env {
		if !reBuildEnv.MatchString(kv) {
			return ref, fmt.Errorf("invalid env entry %q: use KEY=value, one line", kv)
		}
	}
	for _, a := range append(append([]string{}, s.Entrypoint...), s.Cmd...) {
		if strings.ContainsRune(a, 0) || len(a) > 64<<10 {
			return ref, fmt.Errorf("invalid entrypoint/cmd argument")
		}
	}
	if s.User != "" && !reOCIUser.MatchString(s.User) {
		return ref, fmt.Errorf("invalid user %q: use uid[:gid] or name[:group]", s.User)
	}
	if s.MaxMB < 0 || s.MaxMB > 65536 {
		return ref, fmt.Errorf("max_mb out of range (0..65536)")
	}
	return ref, nil
}

func builderOCI(dir string) error { return buildOCI(context.Background(), dir, os.Stdout) }

func buildOCI(ctx context.Context, dir string, log io.Writer) error {
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
	var spec OCISpec
	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &spec); err != nil {
			return fmt.Errorf("spec: %w", err)
		}
	}
	if spec.Arch == "" {
		spec.Arch = runtime.GOARCH
	}
	ref, err := validateOCI(req, spec)
	if err != nil {
		return err
	}
	maxMB := spec.MaxMB
	if maxMB == 0 {
		maxMB = ociDefaultMaxMB
	}
	root := envOr("KLING_ROOT", "/var/lib/kindling")
	lib := envOr("KLING_LIB_DIR", "/usr/local/lib/kindling")
	agent := envOr("KLING_GUEST_AGENT", filepath.Join(lib, "kling-guest"))
	if spec.Arch != runtime.GOARCH {
		if a := os.Getenv("KLING_GUEST_AGENT_" + spec.Arch); a != "" {
			agent = a
		}
	}
	agentSum, err := imagen.SHA256File(agent)
	if err != nil {
		return fmt.Errorf("guest agent: %w (set KLING_GUEST_AGENT or install it with make deploy)", err)
	}
	t := time.Now().UTC().Truncate(time.Second)
	if e := os.Getenv("SOURCE_DATE_EPOCH"); e != "" {
		if s, err := strconv.ParseInt(e, 10, 64); err == nil {
			t = time.Unix(s, 0).UTC()
		}
	}
	// El daemon da KLING_OUT_DIR (y, sin root, KLING_CACHE_DIR): la imagen se
	// deja ahí y él la valida y la mueve a images/ (builders_sinroot.go).
	images := envOr("KLING_OUT_DIR", filepath.Join(root, "images"))
	if err := os.MkdirAll(images, 0o755); err != nil {
		return err
	}
	cache := filepath.Join(root, "cache", "oci")
	if d := os.Getenv("KLING_CACHE_DIR"); d != "" {
		cache = filepath.Join(d, "oci")
	}

	// Cada capa se descomprime una sola vez, a un tar en el directorio de
	// trabajo: el árbol se arma leyendo solo las cabeceras (archive/tar salta
	// los datos con Seek) y el ext4 lee los datos de ahí. Cuesta, mientras
	// dura la construcción, el tamaño descomprimido de las capas en disco (lo
	// que ocupa la imagen, más lo que unas capas pisan de otras), con un tope
	// de 8 veces MaxMB; cada tar se borra en cuanto el ext4 lo ha leído.
	unpacked := filepath.Join(dir, "layers")
	defer os.RemoveAll(unpacked)
	// Sin privilegios (el daemon nos bajó de root), la caché es nuestra: todo lo
	// cacheado se rehashea siempre (oci.Client.SiempreRehash).
	c := &oci.Client{Cache: cache, Log: log, MaxBytes: int64(maxMB) << 20, Unpack: unpacked,
		SiempreRehash: os.Getenv("KLING_BUILD_LIMITS") == "1" && os.Geteuid() != 0}
	tPull := time.Now()
	digest, err := c.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", ref, err)
	}
	img, err := c.Pull(ctx, ref.Name(), digest, spec.Arch)
	if err != nil {
		return err
	}
	cfg := img.Config.Config
	dPull := time.Since(tPull)
	var compressed int64
	for _, l := range img.Layers {
		compressed += l.Size
	}
	logf("%s: %s, %d layer(s), %d MiB compressed", ref, img.ManifestDigest, len(img.Layers), compressed>>20)

	// Aplanar las capas, con sus whiteouts, en un árbol: la raíz entera.
	tTree := time.Now()
	tree := ext4.NewDir(0o755, 0, 0, t)
	var streams []ext4.Stream
	// Tope de entradas mientras se leen: las capas se pisan unas a otras, así
	// que el de ficheros de la raíz final (abajo) es el que cuenta; este solo
	// impide que una capa con millones de entradas agote la memoria antes.
	var entries int64
	for i, l := range img.Layers {
		rc, err := oci.OpenLayer(l)
		if err != nil {
			return err
		}
		err = tree.AddTar(rc, ext4.TarOptions{Stream: len(streams), Whiteouts: true, Time: t,
			Entries: &entries, MaxEntries: 2 * ociMaxFiles})
		rc.Close()
		if err != nil {
			return fmt.Errorf("layer %d (%s): %w", i, l.Digest, err)
		}
		l := l
		streams = append(streams, ext4.TarStream(func() (io.ReadCloser, error) {
			rc, err := oci.OpenLayer(l)
			if err != nil || l.Tar == "" {
				return rc, err
			}
			return removeOnClose{rc, l.Tar}, nil
		}))
	}
	dTree := time.Since(tTree)
	var files, bytes int64
	_ = tree.Walk(func(_ string, n *ext4.Node) error {
		files++
		if n.IsReg() {
			bytes += n.Size
		}
		return nil
	})
	if files > ociMaxFiles || bytes > int64(maxMB)<<23 {
		return fmt.Errorf("image unpacks to %d files and %d MiB, over the limits (%d files, %d MiB)", files, bytes>>20, ociMaxFiles, maxMB*8)
	}
	for _, tool := range ociInitTools {
		if findTool(tree, tool) == "" {
			return fmt.Errorf("the image has no %s; kindling's init needs a shell and %s (distroless images are not supported yet)",
				tool, strings.Join(ociInitTools[1:], ", "))
		}
	}
	if n, _ := tree.Resolve("/entrypoint"); n != nil {
		return fmt.Errorf("the image already has /entrypoint, which kindling's init would run instead of its agent")
	}

	// Lo que se ejecuta: como docker run.
	entry, cmd := cfg.Entrypoint, cfg.Cmd
	if spec.Entrypoint != nil {
		// Como --entrypoint: sustituirlo descarta el CMD de la imagen.
		entry, cmd = spec.Entrypoint, nil
	}
	if spec.Cmd != nil {
		cmd = spec.Cmd
	}
	argv := append(append([]string{}, entry...), cmd...)
	user := cfg.User
	if spec.User != "" {
		user = spec.User
	}
	env := mergeEnv(cfg.Env, spec.Env)
	if len(argv) == 0 {
		logf("the image has no ENTRYPOINT or CMD: only the agent runs (use kling exec)")
	}

	// La identidad de la construcción: mismas entradas, misma imagen.
	h := sha256.New()
	sb, _ := json.Marshal(spec)
	fmt.Fprintf(h, "kindling-oci-v1\x00%s\x00%s\x00%s\x00%s\x00%d\x00", req.Name, img.ManifestDigest, sb, agentSum, t.Unix())
	var id [32]byte
	copy(id[:], h.Sum(nil))

	if err := imagen.PutAgent(tree, agent, spec.Arch, t); err != nil {
		return err
	}
	svc := api.ServiceSpec{Argv: argv, User: user, WorkingDir: cfg.WorkingDir, StopSignal: cfg.StopSignal}
	svcJSON, _ := json.MarshalIndent(svc, "", "  ")
	ociJSON, _ := json.MarshalIndent(map[string]any{"ref": ref.String(), "digest": digest, "manifest": img.ManifestDigest,
		"arch": spec.Arch, "config": cfg}, "", "  ")
	put := []struct {
		p    string
		data string
		mode uint32
	}{
		{"/sbin/overlay-init", scripts.MinimalInit, 0o755},
		{"/entrypoint", imagen.Entrypoint(marcaOCI, env, ""), 0o755},
		{"/etc/resolv.conf", "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", 0o644},
		{"/etc/kindling/oci.json", string(ociJSON) + "\n", 0o644},
		{"/etc/kindling/IMAGE.txt", fmt.Sprintf("kindling_builder=oci\nref=%s\ndigest=%s\nmanifest=%s\nbuilt_at=%s\n",
			ref, digest, img.ManifestDigest, t.Format("2006-01-02T15:04:05Z")), 0o644},
	}
	if len(argv) > 0 {
		put = append(put, struct {
			p    string
			data string
			mode uint32
		}{api.GuestServiceSpec, string(svcJSON) + "\n", 0o644})
	}
	probe, probeWhat := ociReadyProbe(cfg)
	if probe != "" {
		put = append(put, struct {
			p    string
			data string
			mode uint32
		}{api.GuestReadyProbe, probe, 0o755})
	}
	for _, f := range put {
		if err := imagen.Put(tree, f.p, []byte(f.data), f.mode, t); err != nil {
			return err
		}
	}
	if e := imagen.EnvFile(env); e != "" {
		// 0600 de root: el entrypoint es 0755 y lo leería cualquier proceso.
		if err := imagen.Put(tree, imagen.EnvPath, []byte(e), 0o600, t); err != nil {
			return err
		}
	}
	// Los puntos de montaje: la raíz es de solo lectura, el init no puede
	// crearlos si la imagen no los trae.
	for _, d := range []struct {
		p    string
		perm uint32
	}{{"/overlay", 0o755}, {"/rom", 0o755}, {"/run", 0o755}, {"/proc", 0o555}, {"/sys", 0o555}, {"/dev", 0o755}, {"/tmp", 0o1777}} {
		if n, _ := tree.Resolve(d.p); n == nil {
			nd, err := tree.MkdirAll(d.p, d.perm&0o777, 0, 0, t)
			if err != nil {
				return err
			}
			nd.Mode |= d.perm & 0o7000
		}
	}

	tmp := filepath.Join(images, "."+req.Name+".ext4.tmp")
	defer os.Remove(tmp)
	tWrite := time.Now()
	stats, err := writeExt4(tmp, tree, streams, ext4.Options{
		Time: t, UUID: imagen.UUID("kindling-oci", "root", id), LostFound: true, ZeroHoles: true,
		SlackBlocks: 32 << 20 / ext4.BlockSize, SlackInodes: 1024,
	})
	if err != nil {
		return fmt.Errorf("writing the image: %w", err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(images, req.Name+".ext4")); err != nil {
		return err
	}
	logf("image: %d MiB, %d files", stats.Bytes()>>20, stats.Files)
	logf("times: pull and unpack %.1f s, layers %.1f s, ext4 %.1f s", dPull.Seconds(), dTree.Seconds(), time.Since(tWrite).Seconds())

	var layers []map[string]any
	for _, l := range img.Layers {
		layers = append(layers, map[string]any{"digest": l.Digest, "size": l.Size, "media_type": l.MediaType})
	}
	built := map[string]any{"ref": ref.String(), "digest": digest, "manifest": img.ManifestDigest, "arch": spec.Arch,
		"layers": layers, "service": svc, "ready": probeWhat, "ports": sortedKeys(cfg.ExposedPorts),
		"volumes": sortedKeys(cfg.Volumes)}
	if len(cfg.Labels) > 0 {
		built["labels"] = cfg.Labels
	}
	bj, _ := json.Marshal(built)
	// Un contenedor de Docker corre con los núcleos enteros salvo que se le
	// ponga --cpus; el 50 % de un núcleo del daemon está pensado para un
	// servidor MCP que atiende una llamada cada tanto. Con él, un servicio con
	// modelos (Hindsight, medido en el laboratorio) tardaba 43 s en arrancar y
	// 1,4 s por consulta; con un núcleo por vCPU, 19 s y 0,37 s, lo mismo que
	// en Docker. -cpu-pct sigue mandando sobre la receta.
	//
	// El arranque ya lo cubre el impulso hasta la sonda de listo
	// (internal/machine/arranque_cpu.go): con el 50 %, Hindsight está listo en
	// 17,6 s igual que con 200 (44 s sin impulso). Pero la receta se queda por
	// el reposo, que el impulso no toca: cada consulta gasta CPU, y con el 50 %
	// tarda el doble (medido en el lab: recall de Hindsight 0,09 s frente a
	// 0,05 s; una consulta de Postgres que suma 3 M filas, 0,93 s frente a
	// 0,49 s).
	hb, _ := json.MarshalIndent(api.BuildRecipeHints{CPUPctPerVCPU: 100, Built: bj}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "recipe.json"), append(hb, '\n'), 0o644); err != nil {
		return err
	}
	if v := sortedKeys(cfg.Volumes); len(v) > 0 {
		logf("the image declares volumes %s: without -volume they live in the machine's disk", strings.Join(v, " "))
	}
	logf("image %s ready (%s) in %.1f s", req.Name, digest, time.Since(t0).Seconds())
	return nil
}

// removeOnClose borra el tar descomprimido de una capa al cerrarlo: el ext4
// ya no lo vuelve a leer.
type removeOnClose struct {
	io.ReadCloser
	path string
}

func (r removeOnClose) Close() error {
	err := r.ReadCloser.Close()
	os.Remove(r.path)
	return err
}

var marcaOCI = imagen.Marca{Constructor: "oci", Dir: "/etc/kindling", DM: "kindling-layer"}

// mergeEnv son las variables de la imagen con las del spec encima (una
// clave repetida se queda en su sitio con el valor nuevo).
func mergeEnv(image, extra []string) []string {
	out := []string{}
	at := map[string]int{}
	for _, kv := range append(append([]string{}, image...), extra...) {
		k, _, _ := strings.Cut(kv, "=")
		if !reBuildEnv.MatchString(kv) {
			continue // una variable de la imagen que no cabe en una línea de sh
		}
		if i, ok := at[k]; ok {
			out[i] = kv
			continue
		}
		at[k] = len(out)
		out = append(out, kv)
	}
	return out
}

// findTool busca un ejecutable en los directorios de siempre del árbol.
func findTool(tree *ext4.Node, name string) string {
	for _, d := range []string{"/bin", "/sbin", "/usr/bin", "/usr/sbin"} {
		if n, _ := tree.Resolve(d + "/" + name); n != nil && n.IsReg() && n.Mode&0o111 != 0 {
			return d + "/" + name
		}
	}
	return ""
}

// ociReadyProbe es la sonda de "listo" (api.GuestReadyProbe) para la
// configuración: el HEALTHCHECK si lo hay; si no, que acepte conexiones el
// primer puerto TCP de EXPOSE (lo comprueba el agente: no todas las imágenes
// traen nc). Devuelve también qué se comprueba, para la receta.
func ociReadyProbe(cfg oci.ImageConfig) (script, what string) {
	if hc := cfg.Healthcheck; hc != nil && len(hc.Test) > 1 {
		switch hc.Test[0] {
		case "CMD-SHELL":
			return "#!/bin/sh\n# HEALTHCHECK de la imagen (constructor oci de kindling).\n" + hc.Test[1] + "\n", "healthcheck: " + hc.Test[1]
		case "CMD":
			var q []string
			for _, a := range hc.Test[1:] {
				q = append(q, imagen.SQ(a))
			}
			return "#!/bin/sh\n# HEALTHCHECK de la imagen (constructor oci de kindling).\nexec " + strings.Join(q, " ") + "\n",
				"healthcheck: " + strings.Join(hc.Test[1:], " ")
		}
	}
	port := 0
	for p := range cfg.ExposedPorts {
		num, proto, _ := strings.Cut(p, "/")
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 || n > 65535 || (proto != "" && proto != "tcp") {
			continue
		}
		if port == 0 || n < port {
			port = n
		}
	}
	if port == 0 {
		return "", ""
	}
	return fmt.Sprintf("#!/bin/sh\n# EXPOSE %d de la imagen (constructor oci de kindling).\nexec /usr/local/bin/kling-guest -probe-tcp 127.0.0.1:%d\n", port, port),
		fmt.Sprintf("tcp %d", port)
}

func sortedKeys(m map[string]struct{}) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
