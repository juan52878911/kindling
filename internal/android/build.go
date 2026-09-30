package android

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/verity"
	"github.com/juan52878911/kindling/pkg/api"
)

// BaseBuilder es el nombre que llevan en su receta las bases que escribe
// este constructor: solo se sobrescribe una base que lo lleve.
const BaseBuilder = "android-base"

type builder struct {
	spec     Spec
	name     string
	baseName string
	root     string
	lib      string
	agent    string
	cache    string
	work     string
	log      io.Writer
	t        time.Time
	oci      *oci.Client
	id       [32]byte
	errs     []error
}

func (b *builder) logf(format string, a ...any) {
	fmt.Fprintf(b.log, format+"\n", a...)
}

func (b *builder) uuid(kind string) [16]byte {
	s := sha256.Sum256(append([]byte("kindling-android-uuid\x00"+kind+"\x00"), b.id[:]...))
	var u [16]byte
	copy(u[:], s[:16])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

func evalSymlinks(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(r)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Build es el constructor: lee request.json de dir (el directorio de trabajo
// que le da el daemon) y deja en $KLING_ROOT/images la capa <name>.layer.ext4
// (con dm-verity pegado detrás), su base y la receta de la base; en
// dir/recipe.json lo que el daemon tiene que apuntar en la receta de la capa.
func Build(dir string, log io.Writer) error {
	raw, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return err
	}
	var req api.BuildImageRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("request.json: %w", err)
	}
	var spec Spec
	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &spec); err != nil {
			return fmt.Errorf("spec: %w", err)
		}
	}
	if spec.Arch == "" {
		spec.Arch = runtime.GOARCH
	}
	if !reName.MatchString(req.Name) {
		return fmt.Errorf("invalid image name %q", req.Name)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	b := &builder{spec: spec, name: req.Name, log: log, work: dir,
		root: envOr("KLING_ROOT", "/var/lib/kindling"),
		lib:  envOr("KLING_LIB_DIR", "/usr/local/lib/kindling")}
	b.agent = envOr("KLING_GUEST_AGENT", filepath.Join(b.lib, "kling-guest"))
	if spec.Arch != runtime.GOARCH {
		// El agente instalado es el del host; para otra arquitectura hay que
		// dar el suyo.
		if a := os.Getenv("KLING_GUEST_AGENT_" + spec.Arch); a != "" {
			b.agent = a
		}
	}
	b.cache = filepath.Join(b.root, "cache")
	b.oci = &oci.Client{Cache: filepath.Join(b.cache, "oci"), Log: log}
	b.baseName = spec.BaseName
	if b.baseName == "" {
		b.baseName = req.Base
	}
	if b.baseName == "" {
		b.baseName = req.Name + "-base"
	}
	if !reName.MatchString(b.baseName) || b.baseName == req.Name {
		return fmt.Errorf("invalid base name %q", b.baseName)
	}
	b.t = time.Now().UTC().Truncate(time.Second)
	if e := os.Getenv("SOURCE_DATE_EPOCH"); e != "" {
		if s, err := strconv.ParseInt(e, 10, 64); err == nil {
			b.t = time.Unix(s, 0).UTC()
		}
	}
	return b.run(context.Background(), req.Spec)
}

func (b *builder) run(ctx context.Context, rawSpec json.RawMessage) error {
	t0 := time.Now()
	images := filepath.Join(b.root, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		return err
	}
	baseImg := filepath.Join(images, b.baseName+".ext4")
	if _, err := os.Stat(baseImg); err == nil {
		var rec api.ImageRecipe
		rb, _ := os.ReadFile(filepath.Join(images, b.baseName+".recipe.json"))
		if json.Unmarshal(rb, &rec) != nil || rec.Builder != BaseBuilder {
			return fmt.Errorf("base image %q already exists and was not written by the android builder; choose another base_name", b.baseName)
		}
	}
	files, err := b.prepareFiles()
	if err != nil {
		return err
	}
	agentSum, err := sha256Path(b.agent)
	if err != nil {
		return fmt.Errorf("guest agent: %w (set KLING_GUEST_AGENT or install it with make deploy)", err)
	}
	// La identidad de la construcción: con las mismas entradas y la misma
	// hora (SOURCE_DATE_EPOCH) sale la misma imagen, bit a bit, con la misma
	// raíz de verity.
	h := sha256.New()
	fmt.Fprintf(h, "kindling-android-v1\x00%s\x00%s\x00%s\x00%s\x00%d\x00", b.name, b.baseName, rawSpec, agentSum, b.t.Unix())
	fmt.Fprintf(h, "%+v\x00%+v\x00", b.redroid(), debianLock[b.spec.Arch])
	if tr := b.spec.translation(); tr != "" {
		fmt.Fprintf(h, "arm_translation=%s\x00", tr)
		if tr == TranslationLibndk {
			fmt.Fprintf(h, "%+v\x00", libndkPin)
		}
	}
	for _, f := range files {
		fmt.Fprintf(h, "%s=%s\x00", f.spec.Path, f.sha)
	}
	copy(b.id[:], h.Sum(nil))

	layerTmp := filepath.Join(images, "."+b.name+".layer.ext4.tmp")
	baseTmp := filepath.Join(images, "."+b.baseName+".ext4.tmp")
	defer os.Remove(layerTmp)
	defer os.Remove(baseTmp)
	defer b.cleanTemp()

	info, err := b.buildLayer(ctx, layerTmp, files)
	if err != nil {
		return err
	}
	t1 := time.Now()
	table := ""
	built := map[string]any{"arch": b.spec.Arch, "layer": info, "base": b.baseName}
	if b.spec.verity() {
		f, err := os.OpenFile(layerTmp, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		salt := sha256.Sum256(append([]byte("kindling-android-salt\x00"), b.id[:]...))
		res, err := verity.Append(f, uint64(info["layer_blocks"].(int64)), salt[:], b.spec.fecRoots())
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return fmt.Errorf("dm-verity: %w", err)
		}
		table = res.Table("@DEV@")
		b.logf("dm-verity: %d data blocks, %d hash blocks, %d FEC blocks (%d roots), root %x (%.1f s)",
			res.DataBlocks, res.HashBlocks, res.FECBlocks, res.FECRoots, res.RootHash, time.Since(t1).Seconds())
		built["verity_root_hash"] = hex.EncodeToString(res.RootHash)
		built["verity_salt"] = hex.EncodeToString(res.Salt)
		built["verity_fec_roots"] = res.FECRoots
		built["verity_hash"] = "sha256"
		built["verity_table"] = table
	}
	t2 := time.Now()
	baseInfo, err := b.buildBase(ctx, baseTmp, table)
	if err != nil {
		return err
	}
	if len(b.errs) > 0 {
		return b.errs[0]
	}
	built["base_info"] = baseInfo
	b.logf("base written in %.1f s", time.Since(t2).Seconds())

	// Se ponen en su sitio la base antes que la capa: una capa nueva sobre
	// la base vieja no arrancaría (la tabla de verity no cuadra).
	for _, p := range []string{baseTmp, layerTmp} {
		if err := os.Chmod(p, 0o644); err != nil {
			return err
		}
	}
	if err := os.Rename(baseTmp, baseImg); err != nil {
		return err
	}
	if err := os.Rename(layerTmp, filepath.Join(images, b.name+".layer.ext4")); err != nil {
		return err
	}
	baseRec := api.ImageRecipe{Name: b.baseName, Cmd: []string{}, BuiltAt: b.t, Builder: BaseBuilder}
	bs, _ := json.Marshal(map[string]any{"for_layer": b.name, "arch": b.spec.Arch, "info": baseInfo})
	baseRec.Spec = bs
	rb, _ := json.MarshalIndent(baseRec, "", "  ")
	if err := os.WriteFile(filepath.Join(images, b.baseName+".recipe.json"), append(rb, '\n'), 0o644); err != nil {
		return err
	}
	bj, _ := json.Marshal(built)
	hints := api.BuildRecipeHints{Base: b.baseName, CPUPctPerVCPU: 100, GuestIPv6Stack: true, Built: bj}
	hb, _ := json.MarshalIndent(hints, "", "  ")
	if err := os.WriteFile(filepath.Join(b.work, "recipe.json"), append(hb, '\n'), 0o644); err != nil {
		return err
	}
	b.logf("image %s ready: layer + verity over base %s, in %.1f s", b.name, b.baseName, time.Since(t0).Seconds())
	return nil
}

// emptyExt4 escribe un ext4 vacío de mib MiB en el directorio de trabajo
// (el data.ext4 de DATA_MODE=tmpfs: disperso, sin journal, etiqueta "data").
func (b *builder) emptyExt4(mib int) (string, error) {
	p := filepath.Join(b.work, "data.ext4")
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = ext4.Write(f, ext4.NewDir(0o755, 0, 0, b.t), nil, ext4.Options{
		Label: "data", Time: b.t, UUID: b.uuid("data"), LostFound: true,
		MinBlocks: int64(mib) << 20 / ext4.BlockSize, InodeRatio: 16384,
	})
	if err != nil {
		return "", err
	}
	return p, f.Close()
}

func (b *builder) cleanTemp() { os.Remove(filepath.Join(b.work, "data.ext4")) }
