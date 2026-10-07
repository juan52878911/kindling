package main

import (
	"cmp"
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
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
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
//   - o, si a la imagen le faltan las herramientas del script (una
//     distroless, una scratch), el init en Go: /sbin/overlay-init es un
//     enlace a kling-guest, que hace lo mismo que el script y se ejecuta como
//     agente, sin /entrypoint (pkg/guest/init.go);
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
// Por qué el script sigue siendo el init de las imágenes que lo pueden correr,
// y no el de Go para todas: es el que llevan las bases de kindling y todas las
// imágenes de Docker importadas hasta ahora, probado en el laboratorio con
// Postgres, MariaDB, nginx o Hindsight; cambiarlo para ellas no arreglaría
// nada. Los dos hacen lo mismo, y init_test.go (pkg/guest) prueba el de Go
// con los mismos casos que scripts/minimal_init_test.go.
//
// Límites: sin verity (la imagen es la raíz, no una capa); en una imagen sin
// sh, un HEALTHCHECK CMD-SHELL no se puede correr y se sustituye por la sonda
// de EXPOSE.

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
	// Restart es la política de reinicio del servicio (api.Restart*). Por
	// defecto on-failure: Docker no relanza nada sin --restart, y una imagen
	// cuyo CMD acaba con 0 (python:3.12-slim) no se tiene que relanzar para
	// siempre; uno que se cae, sí.
	Restart string `json:"restart,omitempty"`
	// Source dice de dónde salen los blobs: "" es un registro; "archive",
	// un docker save o un layout OCI que el CLI subió a la caché del daemon
	// (kling image import -archive). Entonces Digest es obligatorio (el del
	// manifiesto), Ref es opcional (el nombre que traía la imagen en el
	// archivo) y no se usa la red. Ninguna ruta del host del CLI llega aquí.
	Source string `json:"source,omitempty"`
}

// ociSourceArchive es OCISpec.Source de lo importado de un archivo.
const ociSourceArchive = "archive"

const (
	ociDefaultMaxMB = 4096
	ociMaxFiles     = 2_000_000
)

// ociInitTools son lo que minimal-init.sh necesita de la imagen. El resto lo
// hace con builtins de sh (read, case, echo, [): umount puede faltar.
// scripts/minimal_init_test.go corre sus trozos sin PATH.
var ociInitTools = []string{"sh", "mount", "pivot_root", "mkdir", "ln"}

// reOCIUser es un USER de Docker: uid o nombre, con grupo opcional.
var reOCIUser = lazyre.New(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,31})?$`)

func validateOCI(req api.BuildImageRequest, s OCISpec) (oci.ImageRef, error) {
	if !reBuildName.MatchString(req.Name) {
		return oci.ImageRef{}, fmt.Errorf("invalid image name %q", req.Name)
	}
	if req.Base != "" {
		return oci.ImageRef{}, fmt.Errorf("the oci builder makes its own base; don't give one")
	}
	switch s.Source {
	case "", ociSourceArchive:
	default:
		return oci.ImageRef{}, fmt.Errorf("invalid source %q (want archive, or none for a registry)", s.Source)
	}
	if s.Source == ociSourceArchive && s.Digest == "" {
		return oci.ImageRef{}, fmt.Errorf("an image from an archive needs its manifest digest")
	}
	ref, err := ociSpecRef(s)
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
	switch s.Restart {
	case "", api.RestartAlways, api.RestartOnFailure, api.RestartNo:
	default:
		return ref, fmt.Errorf("invalid restart policy %q: use always, on-failure or no", s.Restart)
	}
	return ref, nil
}

// ociSpecRef es la referencia del spec. Una imagen de un archivo puede no
// tener nombre: entonces es "archive/image" (solo para los mensajes; la imagen
// la fija el digest).
func ociSpecRef(s OCISpec) (oci.ImageRef, error) {
	if s.Source == ociSourceArchive && s.Ref == "" {
		return oci.ImageRef{Registry: "archive", Repo: "image"}, nil
	}
	return oci.ParseImageRef(s.Ref)
}

// ociShown es cómo se nombra la imagen en el registro de la construcción y en
// la receta: la referencia normalizada; la de un archivo, tal como venía
// ("postgres:17-alpine@sha256:...", no de ningún registro), o "archive@..."
// si no traía nombre.
func ociShown(s OCISpec, ref oci.ImageRef) string {
	if s.Source == ociSourceArchive {
		return cmp.Or(s.Ref, "archive") + "@" + ref.Digest
	}
	return ref.String()
}

func builderOCI(dir string) error { return buildOCI(context.Background(), dir, os.Stdout) }

func buildOCI(ctx context.Context, dir string, log io.Writer) error {
	t0 := time.Now()
	logf := func(format string, a ...any) { fmt.Fprintf(log, format+"\n", a...) }
	// Lo primero, y se borra al leerlo: lo que venga después (bajar y parsear
	// tars hostiles) ya no lo tiene en disco.
	auth, err := leerCredencialesRegistro(dir)
	if err != nil {
		return err
	}
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
	lib := envOr("KLING_LIB_DIR", libPorDefecto(root))
	// kling-unpack descomprime las capas más rápido (internal/oci); es un
	// binario del anfitrión, no del invitado, así que no depende de spec.Arch.
	oci.SetUnpackHelper(filepath.Join(lib, "kling-unpack"))
	agent := envOr("KLING_GUEST_AGENT", filepath.Join(lib, "kling-guest"))
	if spec.Arch != runtime.GOARCH {
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
	// cacheado se rehashea siempre (oci.Client.SiempreRehash). Lo que el daemon
	// ya verificó está en KLING_VERIFIED_CACHE_DIR, suya y de solo lectura
	// para nosotros: eso no se rehashea (internal/daemon/builders_cache.go).
	c := &oci.Client{Cache: cache, Log: log, MaxBytes: int64(maxMB) << 20, Unpack: unpacked,
		SiempreRehash: os.Getenv("KLING_BUILD_LIMITS") == "1" && os.Geteuid() != 0, Auth: soloDe(auth, ref.Registry)}
	archive := spec.Source == ociSourceArchive
	if archive {
		// Los blobs los subió el CLI a la caché de root (PUT /oci/blobs): sin
		// red. Sin root, el daemon los dejó en la verificada de este archivo
		// (KLING_VERIFIED_SCOPE_DIR); la de root no la leemos.
		c.Offline = true
	}
	shown := ociShown(spec, ref)
	if os.Getenv("KLING_CACHE_DIR") != "" {
		// La de lo público y, si la hay, la del origen de esta imagen (su
		// registro con credenciales, o el archivo): las de otros orígenes
		// están cerradas para nosotros (builders_cache.go).
		for _, k := range []string{"KLING_VERIFIED_CACHE_DIR", "KLING_VERIFIED_SCOPE_DIR"} {
			if d := os.Getenv(k); d != "" {
				c.Verificadas = append(c.Verificadas, filepath.Join(d, "oci"))
			}
		}
	}
	tPull := time.Now()
	digest, err := c.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", shown, err)
	}
	img, err := c.Pull(ctx, ref.Name(), digest, spec.Arch)
	if err != nil {
		return err
	}
	if os.Getenv("KLING_CACHE_DIR") != "" {
		// Para el daemon: qué blobs de la caché usó esta construcción. Si
		// acaba bien, los comprueba él y pasa a la verificada los nuevos.
		usados := strings.Join(c.Usados(), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "cache-used"), []byte(usados), 0o600); err != nil {
			return err
		}
	}
	cfg := img.Config.Config
	dPull := time.Since(tPull)
	var compressed int64
	for _, l := range img.Layers {
		compressed += l.Size
	}
	how := "compressed"
	if archive {
		how = "of layers" // un docker save las trae sin comprimir
	}
	logf("%s: %s, %d layer(s), %d MiB %s", shown, img.ManifestDigest, len(img.Layers), compressed>>20, how)
	if h := oci.UnpackHelper(); h != "" {
		logf("compressed layers are decompressed with %s", h)
	}

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
	// El init: el script si la imagen trae lo que necesita; si no (una
	// distroless, una scratch), el init en Go de kling-guest. También si la
	// imagen trae su propio /entrypoint, que el script ejecutaría en vez del
	// agente: el de Go no lo mira.
	var missing []string
	for _, tool := range ociInitTools {
		if findTool(tree, tool) == "" {
			missing = append(missing, tool)
		}
	}
	ownEntry, _ := tree.Resolve("/entrypoint")
	goInit := len(missing) > 0 || ownEntry != nil
	switch {
	case len(missing) > 0:
		logf("the image has no %s: it boots with kindling's init in Go", strings.Join(missing, ", "))
	case goInit:
		logf("the image has its own /entrypoint: it boots with kindling's init in Go, which leaves it alone")
	}
	// Las sondas de listo son scripts con #!/bin/sh si la imagen lo tiene.
	hasSh := treeExec(tree, "/bin/sh")
	probeCfg := cfg
	if hc := cfg.Healthcheck; !hasSh && hc != nil && len(hc.Test) > 1 && hc.Test[0] == "CMD-SHELL" {
		// Docker la correría con sh y nunca pasaría; aquí se queda la de EXPOSE.
		logf("warning: the image's HEALTHCHECK needs a shell, which the image doesn't have; ignoring it")
		probeCfg.Healthcheck = nil
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
	env, fuera := mergeEnv(cfg.Env, spec.Env)
	for _, k := range fuera {
		// Solo el nombre: el valor puede ser un secreto.
		logf("warning: the image's ENV %s is not KEY=value without NUL or CR; left out", k)
	}
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
	if goInit {
		// Después de PutAgent, que ya dijo si es de otra arquitectura.
		if err := agentIsInit(agent); err != nil {
			return err
		}
	}
	svc := api.ServiceSpec{Argv: argv, User: user, WorkingDir: cfg.WorkingDir, StopSignal: cfg.StopSignal,
		Restart: cmp.Or(spec.Restart, api.RestartOnFailure)}
	if _, err := api.ParseSignal(svc.StopSignal); err != nil {
		// El agente la cambiaría igual por SIGTERM; aquí se ve al importar.
		logf("warning: STOPSIGNAL: %v; the service will be stopped with SIGTERM", err)
		svc.StopSignal = ""
	}
	svc.ProbeTimeoutSeconds, svc.ReadyStartPeriodSeconds = ociReadyTimes(probeCfg.Healthcheck)
	svcJSON, _ := json.MarshalIndent(svc, "", "  ")
	ociInfo := map[string]any{"ref": shown, "digest": digest, "manifest": img.ManifestDigest,
		"arch": spec.Arch, "config": cfg}
	sourceLine := ""
	if archive {
		ociInfo["source"] = ociSourceArchive
		sourceLine = "source=" + ociSourceArchive + "\n"
	}
	ociJSON, _ := json.MarshalIndent(ociInfo, "", "  ")
	initKind := "sh"
	if goInit {
		initKind = "go"
	}
	type putFile struct {
		p    string
		data string
		mode uint32
	}
	put := []putFile{
		{"/etc/resolv.conf", "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", 0o644},
		{"/etc/kindling/oci.json", string(ociJSON) + "\n", 0o644},
		{"/etc/kindling/IMAGE.txt", fmt.Sprintf("kindling_builder=oci\n%sref=%s\ndigest=%s\nmanifest=%s\ninit=%s\nbuilt_at=%s\n",
			sourceLine, shown, digest, img.ManifestDigest, initKind, t.Format("2006-01-02T15:04:05Z")), 0o644},
	}
	if goInit {
		// kling-guest hace de init al verse llamado overlay-init
		// (pkg/guest/init.go): carga /etc/kling/env y se ejecuta como agente.
		if err := imagen.Link(tree, "/sbin/overlay-init", "/usr/local/bin/kling-guest", t); err != nil {
			return err
		}
	} else {
		put = append(put, putFile{"/sbin/overlay-init", scripts.MinimalInit, 0o755},
			putFile{"/entrypoint", imagen.Entrypoint(marcaOCI, env, ""), 0o755})
	}
	if len(argv) > 0 {
		put = append(put, putFile{api.GuestServiceSpec, string(svcJSON) + "\n", 0o644})
	}
	probe, probeWhat := ociReadyProbe(probeCfg, hasSh)
	if probe != "" {
		put = append(put, putFile{api.GuestReadyProbe, probe, 0o755})
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
	built := map[string]any{"ref": shown, "digest": digest, "manifest": img.ManifestDigest, "arch": spec.Arch,
		"layers": layers, "service": svc, "ready": probeWhat, "init": initKind, "ports": sortedKeys(cfg.ExposedPorts),
		"volumes": sortedKeys(cfg.Volumes)}
	if len(cfg.Labels) > 0 {
		built["labels"] = cfg.Labels
	}
	if archive {
		// De dónde vino: un archivo, y el id de la imagen (el digest de su
		// configuración, el que enseña docker images). Sin rutas del host.
		built["source"] = ociSourceArchive
		built["config"] = img.Manifest.Config.Digest
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

// ficheroCredencialesRegistro es el fichero con las credenciales del registro
// que deja el daemon en el directorio de trabajo
// (internal/daemon/registries.go): {"<registro>": {"username", "password"}}.
const ficheroCredencialesRegistro = "registry-auth.json"

// leerCredencialesRegistro lee y borra el fichero de credenciales, si lo hay.
// Sin seguir enlaces y solo un fichero regular. Ningún error cita su
// contenido.
func leerCredencialesRegistro(dir string) (map[string]oci.Credential, error) {
	p := filepath.Join(dir, ficheroCredencialesRegistro)
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ficheroCredencialesRegistro, err)
	}
	defer f.Close()
	os.Remove(p)
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", ficheroCredencialesRegistro)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ficheroCredencialesRegistro, err)
	}
	var m map[string]oci.Credential
	if json.Unmarshal(b, &m) != nil {
		return nil, fmt.Errorf("%s is not valid JSON", ficheroCredencialesRegistro)
	}
	return m, nil
}

// soloDe deja de auth solo las credenciales de registry: el daemon ya manda
// solo esas, y el cliente OCI solo las usa con él, pero no cuesta nada.
func soloDe(auth map[string]oci.Credential, registry string) map[string]oci.Credential {
	k, err := oci.CredentialKey(registry)
	if err != nil {
		return nil
	}
	if c, ok := auth[k]; ok {
		return map[string]oci.Credential{k: c}
	}
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
// clave repetida se queda en su sitio con el valor nuevo), y los nombres de
// las de la imagen que se dejan fuera. Un valor de la imagen puede tener
// saltos de línea (Docker los deja en el ENV): imagen.SQ los guarda entre
// comillas, y tanto sh como el init en Go los leen así. Un NUL o un CR, no.
// Las del spec ya llegan validadas, de una línea (validateOCI).
func mergeEnv(image, extra []string) (out, fuera []string) {
	out = []string{}
	at := map[string]int{}
	for _, kv := range append(append([]string{}, image...), extra...) {
		k, v, _ := strings.Cut(kv, "=")
		if !reBuildEnv.MatchString(k+"=") || strings.ContainsAny(v, "\x00\r") {
			fuera = append(fuera, k)
			continue
		}
		if i, ok := at[k]; ok {
			out[i] = kv
			continue
		}
		at[k] = len(out)
		out = append(out, kv)
	}
	return out, fuera
}

// findTool busca un ejecutable en los directorios de siempre del árbol.
func findTool(tree *ext4.Node, name string) string {
	for _, d := range []string{"/bin", "/sbin", "/usr/bin", "/usr/sbin"} {
		if treeExec(tree, d+"/"+name) {
			return d + "/" + name
		}
	}
	return ""
}

// treeExec dice si p es un ejecutable del árbol (siguiendo enlaces).
func treeExec(tree *ext4.Node, p string) bool {
	n, _ := tree.Resolve(p)
	return n != nil && n.IsReg() && n.Mode&0o111 != 0
}

// ociReadyTimes son el Timeout y el StartPeriod del HEALTHCHECK (en
// nanosegundos) en segundos, redondeados hacia arriba y con tope
// api.MaxReadyTimeoutSeconds. El Interval y los Retries no se usan: "listo" es
// la primera vez que la sonda pasa, y quien espera la pregunta a su ritmo.
func ociReadyTimes(hc *oci.Healthcheck) (timeout, startPeriod int) {
	if hc == nil || len(hc.Test) < 2 || (hc.Test[0] != "CMD" && hc.Test[0] != "CMD-SHELL") {
		return 0, 0
	}
	secs := func(ns int64) int {
		if ns <= 0 {
			return 0
		}
		return int(min((ns+int64(time.Second)-1)/int64(time.Second), api.MaxReadyTimeoutSeconds))
	}
	return secs(hc.Timeout), secs(hc.StartPeriod)
}

// agentIsInit comprueba que el agente que va a la imagen sabe hacer de init
// (guest.InitMarker). Uno anterior, el de KLING_GUEST_AGENT o el de otra
// arquitectura que kling upgrade no cambia, se metería sin queja y la imagen
// no arrancaría: correría como PID 1 sin overlay ni /proc, y sin decir por qué.
func agentIsInit(agent string) error {
	f, err := os.Open(agent)
	if err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	defer f.Close()
	ok, err := guest.HasInitMarker(f)
	if err != nil {
		return fmt.Errorf("guest agent %s: %w", agent, err)
	}
	if !ok {
		return fmt.Errorf("the guest agent at %s predates the Go init this image needs; update it "+
			"(make deploy, or kling upgrade if it lives in KLING_LIB_DIR; KLING_GUEST_AGENT and KLING_GUEST_AGENT_<arch> pick another one)", agent)
	}
	return nil
}

// ociReadyProbe es la sonda de "listo" (api.GuestReadyProbe) para la
// configuración: el HEALTHCHECK si lo hay; si no, que acepte conexiones el
// primer puerto TCP de EXPOSE (lo comprueba el agente: no todas las imágenes
// traen nc). Devuelve también qué se comprueba, para la receta.
//
// Sin sh en la imagen (sh = false), la sonda no es un script: es un #! que
// apunta al agente, que el kernel ejecuta sin shell; el HEALTHCHECK CMD va en
// JSON en la segunda línea (kling-guest -exec-json) y un CMD-SHELL no se puede
// correr (lo descarta quien llama).
func ociReadyProbe(cfg oci.ImageConfig, sh bool) (script, what string) {
	if hc := cfg.Healthcheck; hc != nil && len(hc.Test) > 1 {
		switch {
		case !sh && hc.Test[0] == "CMD":
			argv, _ := json.Marshal(hc.Test[1:])
			return "#!/usr/local/bin/kling-guest -exec-json\n" + string(argv) + "\n", "healthcheck: " + strings.Join(hc.Test[1:], " ")
		case !sh:
		case hc.Test[0] == "CMD-SHELL":
			return "#!/bin/sh\n# HEALTHCHECK de la imagen (constructor oci de kindling).\n" + hc.Test[1] + "\n", "healthcheck: " + hc.Test[1]
		case hc.Test[0] == "CMD":
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
	if !sh {
		// El #! pasa un único argumento: -probe-tcp=..., con el "=".
		return fmt.Sprintf("#!/usr/local/bin/kling-guest -probe-tcp=127.0.0.1:%d\n", port), fmt.Sprintf("tcp %d", port)
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

// pistaAgente dice cómo conseguir el agente del invitado (un binario de
// Linux) donde lo busca el constructor. En macOS no hay make deploy: lo pone
// make install, o se compila y se señala con KLING_GUEST_AGENT.
func pistaAgente() string {
	if runtime.GOOS == "darwin" {
		return "on macOS it is a Linux binary: make install puts it in lib/ of the data root, " +
			"or build it with make guest GOARCH=arm64 and set KLING_GUEST_AGENT to it"
	}
	return "set KLING_GUEST_AGENT or install it with make deploy"
}

// libPorDefecto es dónde buscan los constructores el agente y los scripts:
// /usr/local/lib/kindling en Linux (lo instala make deploy, de root) y, en
// macOS, lib/ de la raíz de datos, porque allí el daemon es tu usuario y lo
// pone make install sin sudo.
func libPorDefecto(root string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(root, "lib")
	}
	return "/usr/local/lib/kindling"
}
