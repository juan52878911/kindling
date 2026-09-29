package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El dorado: Android arrancado en frío hasta que la sonda de la imagen dice
// "listo" (sys.boot_completed y la pantalla preparada), comprobado y guardado
// con `save`. Cada teléfono es un `run -from` de él.
//
// Antes de guardar se comprueba lo que un dorado reparte a todos sus clones:
// que la caché de páginas del invitado dice lo mismo que el disco (en el Mac
// bajo presión se han visto páginas a ceros: SIGILL en cada proceso nuevo) y
// que ningún proceso de Android se cayó arrancando. Todo por la API de
// kling-phoned, con un token de un solo uso que se revoca antes de guardar:
// el dorado sale sin ningún token y su API cerrada.

type goldenOpts struct {
	Name    string
	Image   string
	CPUs    int
	MemMiB  int
	Egress  string
	Replace bool
	Keep    bool
}

// goldenInfo es la anotación "phone" del snapshot del dorado.
type goldenInfo struct {
	Image          string     `json:"image"`
	Arch           string     `json:"arch,omitempty"`
	Kernel         string     `json:"kernel,omitempty"`
	Phoned         string     `json:"phoned,omitempty"`
	Verity         string     `json:"verity,omitempty"`
	VerityRootHash string     `json:"verity_root_hash,omitempty"`
	Net            string     `json:"net,omitempty"`
	AdbSecure      bool       `json:"adb_secure"`
	Uidump         bool       `json:"uidump"`
	VCPUs          int        `json:"vcpus"`
	MemMiB         int        `json:"mem_mib"`
	Egress         string     `json:"egress"`
	BootSeconds    float64    `json:"boot_seconds"`
	SaveSeconds    float64    `json:"save_seconds"`
	BuiltAt        time.Time  `json:"built_at"`
	Daemon         string     `json:"daemon_version,omitempty"`
	Build          *verifyRec `json:"build_check"`
	Verify         *verifyRec `json:"last_verify,omitempty"`
}

// verifyRec es el resultado de una comprobación (al construir o `verify`).
type verifyRec struct {
	OK         bool      `json:"ok"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	Mismatches []string  `json:"mismatches,omitempty"`
	Errors     int       `json:"errors,omitempty"`
	Seconds    float64   `json:"seconds"`
	Crashes    []string  `json:"crashes,omitempty"`
	At         time.Time `json:"at"`
}

func cmdGolden(args []string) error {
	if len(args) < 1 {
		return usageErr(errors.New("usage: kling phone golden build|verify|inspect [-name G]"))
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("golden "+sub, flag.ContinueOnError)
	host := hostFlag(fs)
	name := fs.String("name", "", "golden template (default: phone.golden, or phone-golden)")
	var image, egress *string
	var cpus, mem *int
	var replace, keep, js *bool
	switch sub {
	case "build":
		image = fs.String("image", "", "Android image (default: phone.image, or android13)")
		cpus = fs.Int("cpus", 0, "vCPUs (default: phone.cpus, or 2)")
		mem = fs.Int("mem", 0, "MiB (default: phone.mem, or 1536)")
		egress = fs.String("egress", "", "egress of the golden and its phones: none | internet")
		replace = fs.Bool("replace", false, "replace an existing golden (it must have no phones)")
		keep = fs.Bool("keep", false, "keep the cold machine if a check fails")
	case "verify":
	case "inspect":
		js = fs.Bool("json", false, "the annotation as JSON")
	default:
		return usageErr(fmt.Errorf("unknown golden subcommand %q: use build, verify or inspect", sub))
	}
	pos, err := parseFlags(fs, rest)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr(fmt.Errorf("golden %s takes no arguments (the name goes in -name)", sub))
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	g := or(*name, a.s.Golden)
	switch sub {
	case "build":
		o := goldenOpts{Name: g, Image: or(*image, a.s.Image), CPUs: a.s.CPUs, MemMiB: a.s.MemMiB,
			Egress: or(*egress, a.s.Egress), Replace: *replace, Keep: *keep}
		if *cpus > 0 {
			o.CPUs = *cpus
		}
		if *mem > 0 {
			o.MemMiB = *mem
		}
		info, err := a.goldenBuild(ctx, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "golden %s: %s, kernel %s, verity %s, page cache %d files ok, boot %.1f s, save %.1f s\n",
			g, info.Arch, dash(info.Kernel), dash(info.Verity), info.Build.Files, info.BootSeconds, info.SaveSeconds)
		return nil
	case "verify":
		v, err := a.goldenVerify(ctx, g)
		if v != nil {
			fmt.Fprintf(a.out, "golden %s: page cache %d files, %d mismatches, %d crashes (%.1f s)\n",
				g, v.Files, len(v.Mismatches), len(v.Crashes), v.Seconds)
		}
		return err
	default:
		return a.goldenInspect(ctx, g, *js)
	}
}

// Señales de un Android roto en el búfer de fallos: SIGILL (una página a
// ceros), una caída de system_server o un binario corrupto.
var reCrash = regexp.MustCompile(`SIGILL|IN SYSTEM PROCESS|bad ELF magic`)

// check es verify-cache + el búfer de fallos, con un token de un solo uso ya
// entregado al teléfono.
func (a *app) check(ctx context.Context, m *api.Machine, tok string) (*verifyRec, error) {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	r, err := a.call(vctx, m, tok, "POST", "/v1/verify-cache", nil, false)
	var ae *apiError
	if err != nil && !(errors.As(err, &ae) && ae.Status == 409) {
		return nil, fmt.Errorf("verify-cache: %w", err)
	}
	rec := &verifyRec{At: a.now().UTC()}
	if jerr := json.Unmarshal(r.Body, rec); jerr != nil {
		return nil, fmt.Errorf("verify-cache: %v", jerr)
	}
	rec.At = a.now().UTC()
	lr, err := a.call(ctx, m, tok, "GET", "/v1/logs?buffer=crash&lines=400", nil, false)
	if err != nil {
		return rec, fmt.Errorf("crash log: %w", err)
	}
	for _, l := range strings.Split(string(lr.Body), "\n") {
		if reCrash.MatchString(l) && len(rec.Crashes) < 5 {
			rec.Crashes = append(rec.Crashes, strings.TrimSpace(l))
		}
	}
	rec.OK = rec.OK && len(rec.Crashes) == 0
	return rec, nil
}

// withTempToken da al teléfono un token de control de un solo uso (documento
// solo de tokens), llama a fn y lo revoca (lista vacía: API cerrada).
func (a *app) withTempToken(ctx context.Context, m *api.Machine, fn func(tok string) error) error {
	tok, err := newToken()
	if err != nil {
		return err
	}
	toks := []apiToken{{SHA256: tokenHash(tok), Scope: "control"}}
	if err := a.applyDoc(ctx, m, identityDoc{Phone: phoneDoc{APITokens: &toks}}); err != nil {
		return err
	}
	ferr := fn(tok)
	none := []apiToken{}
	if err := a.applyDoc(context.WithoutCancel(ctx), m, identityDoc{Phone: phoneDoc{APITokens: &none}}); err != nil && ferr == nil {
		ferr = fmt.Errorf("revoking the temporary token: %w", err)
	}
	// Revocado de verdad: el mismo token ya no abre.
	if ferr == nil {
		if _, err := a.call(ctx, m, tok, "GET", "/v1/tree", nil, false); err == nil {
			ferr = errors.New("the temporary API token still works after revoking it")
		} else if ae := (*apiError)(nil); !errors.As(err, &ae) || ae.Status != 401 {
			ferr = fmt.Errorf("checking the revocation: %w", err)
		}
	}
	return ferr
}

func (a *app) goldenBuild(ctx context.Context, o goldenOpts) (*goldenInfo, error) {
	if o.CPUs < 1 || o.MemMiB < 256 {
		return nil, fmt.Errorf("golden: bad resources (%d vCPU, %d MiB)", o.CPUs, o.MemMiB)
	}
	if _, err := a.d.Snapshot(ctx, o.Name); err == nil && !o.Replace {
		return nil, fmt.Errorf("golden %s already exists (-replace, with no phones from it)", o.Name)
	}
	cold := o.Name + "-build"
	if m, err := a.d.Get(ctx, cold); err == nil {
		if err := a.d.Remove(ctx, m.ID); err != nil {
			return nil, err
		}
	}
	if err := a.waitMemory(ctx); err != nil {
		return nil, err
	}
	info := &goldenInfo{Image: o.Image, VCPUs: o.CPUs, MemMiB: o.MemMiB, Egress: o.Egress}
	if inf, err := a.d.Info(ctx); err == nil {
		info.Arch, info.Daemon = inf.Arch, inf.Version
	}
	if rc, err := a.d.ImageRecipe(ctx, o.Image); err == nil && len(rc.Spec) > 0 {
		var spec struct {
			VerityRoot string `json:"verity_root_hash"`
			Arch       string `json:"arch"`
		}
		_ = json.Unmarshal(rc.Spec, &spec)
		info.VerityRootHash = spec.VerityRoot
		if info.Arch == "" {
			info.Arch = spec.Arch
		}
	}
	fmt.Fprintf(a.errw, "==> golden: cold boot of %s (%d vCPU, %d MiB, CPU %d %%, egress %s)\n",
		o.Image, o.CPUs, o.MemMiB, o.CPUs*100, o.Egress)
	t0 := a.now()
	m, err := a.d.Run(ctx, api.RunRequest{
		Image: o.Image, Name: cold, VCPUs: o.CPUs, MemMiB: o.MemMiB,
		// vz pausa la VM entera para cumplir un techo: con el 50 % por defecto
		// Android va a un cuarto de velocidad.
		CPUPct: o.CPUs * 100, Egress: o.Egress, TTLSeconds: 6 * 3600,
		WaitReady: true, ReadyTimeoutSeconds: 240,
		Labels: map[string]string{
			api.LabelPorts: strconv.Itoa(adbPort) + "," + strconv.Itoa(phonedPort),
			labelPhone:     "1",
			labelGolden:    o.Name,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("cold boot of %s: %w", o.Image, err)
	}
	drop := func(err error) (*goldenInfo, error) {
		if o.Keep {
			return nil, fmt.Errorf("%w (%s kept for inspection)", err, cold)
		}
		_ = a.d.Remove(context.WithoutCancel(ctx), m.ID)
		return nil, err
	}
	h, err := a.waitHealthy(ctx, m, 90*time.Second)
	if err != nil {
		if logs, lerr := a.d.Logs(ctx, m.ID, 30); lerr == nil {
			fmt.Fprintln(a.errw, logs)
		}
		return drop(fmt.Errorf("golden: Android is not healthy after booting: %w", err))
	}
	info.BootSeconds = a.now().Sub(t0).Seconds()
	info.Kernel, info.Phoned, info.Verity, info.Net = h.Kernel, h.Version, h.Verity, h.Net
	info.AdbSecure, info.Uidump = h.AdbSecure, h.Uidump
	fmt.Fprintf(a.errw, "==> golden: ready in %.1f s (kernel %s, verity %s, net %s); checking the page cache\n",
		info.BootSeconds, dash(h.Kernel), dash(h.Verity), dash(h.Net))

	var rec *verifyRec
	if err := a.withTempToken(ctx, m, func(tok string) error {
		var err error
		rec, err = a.check(ctx, m, tok)
		return err
	}); err != nil {
		return drop(fmt.Errorf("golden: %w", err))
	}
	info.Build = rec
	if !rec.OK {
		return drop(fmt.Errorf("golden: not saving a broken golden: page cache %d mismatches %v, crashes %v "+
			"(host memory pressure? retry with more free memory)", len(rec.Mismatches), rec.Mismatches, rec.Crashes))
	}
	fmt.Fprintf(a.errw, "==> golden: page cache = disk (%d files, %.0f MiB, %.1f s), no crashes; saving\n",
		rec.Files, float64(rec.Bytes)/(1<<20), rec.Seconds)

	t1 := a.now()
	if _, err := a.d.Commit(ctx, m.ID, o.Name, o.Replace); err != nil {
		return drop(fmt.Errorf("save %s: %w", o.Name, err))
	}
	info.SaveSeconds = a.now().Sub(t1).Seconds()
	info.BuiltAt = a.now().UTC()
	_ = a.d.Remove(context.WithoutCancel(ctx), m.ID)
	if _, err := a.d.SetAnnotation(ctx, o.Name, annGolden, info); err != nil {
		return info, fmt.Errorf("golden saved, but its annotation was not: %w", err)
	}
	return info, nil
}

// goldenVerify comprueba el dorado en un clon de usar y tirar (sin identidad):
// el dorado mismo no se toca.
func (a *app) goldenVerify(ctx context.Context, golden string) (*verifyRec, error) {
	snap, err := a.d.Snapshot(ctx, golden)
	if err != nil {
		return nil, err
	}
	var info goldenInfo
	if _, err := snap.Annotation(annGolden, &info); err != nil {
		return nil, err
	}
	if err := a.waitMemory(ctx); err != nil {
		return nil, err
	}
	name := golden + "-verify"
	if m, err := a.d.Get(ctx, name); err == nil {
		_ = a.d.Remove(ctx, m.ID)
	}
	m, err := a.d.Run(ctx, api.RunRequest{From: golden, Name: name, TTLSeconds: 3600, Egress: or(snap.Egress, "none"),
		WaitReady: true, ReadyTimeoutSeconds: 180})
	if err != nil {
		return nil, err
	}
	defer func() { _ = a.d.Remove(context.WithoutCancel(ctx), m.ID) }()
	if _, err := a.waitHealthy(ctx, m, 60*time.Second); err != nil {
		return nil, err
	}
	var rec *verifyRec
	if err := a.withTempToken(ctx, m, func(tok string) error {
		var err error
		rec, err = a.check(ctx, m, tok)
		return err
	}); err != nil {
		return nil, err
	}
	info.Verify = rec
	if _, err := a.d.SetAnnotation(ctx, golden, annGolden, &info); err != nil {
		return rec, err
	}
	if !rec.OK {
		return rec, fmt.Errorf("golden %s is broken: %d page-cache mismatches %v, crashes %v", golden, len(rec.Mismatches), rec.Mismatches, rec.Crashes)
	}
	return rec, nil
}

func (a *app) goldenInspect(ctx context.Context, golden string, js bool) error {
	snap, err := a.d.Snapshot(ctx, golden)
	if err != nil {
		return err
	}
	var info goldenInfo
	ok, err := snap.Annotation(annGolden, &info)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s has no %q annotation (not built by kling phone golden build)", golden, annGolden)
	}
	if js {
		b, _ := json.MarshalIndent(&info, "", "  ")
		fmt.Fprintln(a.out, string(b))
		return nil
	}
	fmt.Fprintf(a.out, "golden:   %s (image %s, %d vCPU, %d MiB, egress %s, %d phone(s))\n", golden, info.Image, info.VCPUs, info.MemMiB, info.Egress, snap.Instances)
	fmt.Fprintf(a.out, "arch:     %s   kernel %s   kling-phoned %s\n", dash(info.Arch), dash(info.Kernel), dash(info.Phoned))
	vh := ""
	if info.VerityRootHash != "" {
		vh = " (root " + info.VerityRootHash[:min(16, len(info.VerityRootHash))] + "…)"
	}
	fmt.Fprintf(a.out, "verity:   %s%s   net %s   adb secure %v   uidump %v\n", dash(info.Verity), vh, dash(info.Net), info.AdbSecure, info.Uidump)
	fmt.Fprintf(a.out, "built:    %s (boot %.1f s, save %.1f s)\n", info.BuiltAt.Format(time.RFC3339), info.BootSeconds, info.SaveSeconds)
	for _, v := range []struct {
		what string
		r    *verifyRec
	}{{"check", info.Build}, {"verify", info.Verify}} {
		if v.r == nil {
			continue
		}
		fmt.Fprintf(a.out, "%-9s %v at %s: %d files, %d mismatches, %d crashes (%.1f s)\n", v.what+":", v.r.OK,
			v.r.At.Format(time.RFC3339), v.r.Files, len(v.r.Mismatches), len(v.r.Crashes), v.r.Seconds)
	}
	return nil
}
