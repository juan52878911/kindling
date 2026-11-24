package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/internal/aigw"
	"github.com/juan52878911/kindling/internal/dorado"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/units"
)

// `kling ai model deploy`: cualquier servidor HTTP como backend de modelo del
// gateway (plan de la ronda 8, §1.3). Hace el dorado versionado de una imagen
// ya construida (<modelo>-v<n>) con internal/dorado y escribe su bloque
// "kind": "http" en ai.json. Un modelo que ya está en el registro no se toca:
// se hace la versión nueva y `kling ai model roll` la pone a servir.

// deployNameRE es el nombre del modelo: el de aigw (minúsculas, dígitos, . _ -)
// con sitio para el sufijo -v<n> dentro de los 64 caracteres de un snapshot.
var deployNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,55}$`)

// Sin chispa-classify: el gateway no lo admite en un bloque kind http (es la
// réplica de kind chispa con backend microvm, que hace `kling ai chispa
// deploy`), y el bloque escrito no cargaría.
var deployContracts = map[string]bool{
	aigw.ContractOpenAI: true, aigw.ContractOpenAIEmbeddings: true,
	aigw.ContractChispaDecide: true, aigw.ContractRaw: true,
}

var deployIdle = map[string]bool{"": true, aigw.IdleRemove: true, aigw.IdlePause: true, aigw.IdleFreeze: true}

type modelDeployOptions struct {
	Name, Image   string
	Port          int
	Contract      string
	Routes        []string
	Health        aigw.HealthConfig
	Warmup        []dorado.Request
	VCPUs, MemMiB int
	CPUPct        int
	Idle          string
	MaxInflight   int
	MaxReplicas   int
	Env           []string
	AllowEnv      bool
	AllowExec     bool
	Wait          time.Duration
	Config        string // ai.json

	// Ronda 9. ModelBytes da el plazo automático (Wait 0) y el 100 % del
	// progreso; Stall corta una carga que no avanza (0 = 2 min).
	ModelBytes int64
	Stall      time.Duration
	// WarmupKey: clave de calentamiento desechable (P2).
	WarmupKey bool
	// Tenants, Alias y Shared van al bloque (P4): un dorado por inquilino.
	Tenants []string
	Alias   string
	Shared  bool
	// AllowDomains y Credentials son la red y las credenciales del proxy de
	// la plantilla y de sus réplicas (P5).
	AllowDomains []string
	Credentials  []api.CredentialSpec
}

type modelDeployResult struct {
	Snapshot string
	Version  int
	// Written dice si se escribió el bloque en ai.json (modelo nuevo); si el
	// modelo ya estaba, la versión espera a `kling ai model roll`.
	Written bool
	Golden  *dorado.Result
}

func modelsDeploy(args []string) error {
	fs := flag.NewFlagSet("ai model deploy", flag.ContinueOnError)
	host := hostFlag(fs)
	cfgPath := fs.String("config", aiDefault("ai.json"), "model and task registry to write the block to")
	image := fs.String("image", "", "image with the server (required): built or imported on this daemon, its files baked in")
	port := fs.Int("port", aigw.DefaultBackendPort, "port the server listens on inside the machine")
	contract := fs.String("contract", "", "openai, openai-embeddings, chispa-decide or raw (required)")
	health := fs.String("health", "", "GET path that answers when the server is ready, e.g. /healthz (default: the port accepting connections)")
	expect := fs.Int("expect", 0, "status the -health path answers when ready (default 200)")
	healthTimeout := fs.Duration("health-timeout", 0, "timeout of each health probe (default 5s)")
	warmup := fs.String("warmup", "", "JSONL file of warm-up requests ({\"method\":\"POST\",\"path\":\"/v1/decide\",\"headers\":{\"Accept\":\"application/json\"},\"body\":{...}}); each must answer 2xx; no credential headers (see -warmup-key)")
	warmupKey := fs.Bool("warmup-key", false, "warm up a backend that cannot run open with a disposable key: headers may use {{warmup_key}}, the server gets only KLING_WARMUP_KEY_SHA256 and KLING_WARMUP_KEY_EXPIRES and must reject the key after that time")
	cpus := fs.Int("cpus", 2, "microVM vCPUs (fixed in the golden snapshot)")
	mem := units.MiBVar(fs, "mem", 1024, "microVM memory: 1G, 2048M (bare number = MiB). Size it to the server's peak after loading plus a margin: the golden dumps all of it and every cold restore reads it back, so extra memory costs time and disk")
	modelBytes := fs.Int64("model-bytes", 0, "size of the model's weights in bytes: sets the load wait (with -wait 0) and the progress percentage")
	stall := fs.Duration("stall", 0, "give up when the server touches no new memory and uses no CPU for this long (default 2m)")
	alias := fs.String("alias", "", "common name for the per-tenant blocks of one service: clients call /v1/m/<alias>/... and reach their tenant's golden")
	shared := fs.Bool("shared", false, "chispa-decide: several -tenant share this golden and its banks, knowingly")
	cpuPct := fs.Int("cpu-pct", 0, "CPU ceiling as a percentage of one core, kept by the snapshot (default: 100 per vCPU)")
	idle := fs.String("idle", "", "what the gateway does with an idle replica: remove, pause or freeze (default: the contract's)")
	maxInflight := fs.Int("max-inflight", 0, "requests at once per replica (default: the gateway's)")
	maxReplicas := fs.Int("max-replicas", 0, "replica cap (default: the gateway's)")
	allowEnv := fs.Bool("allow-env", false, "accept -e names that look like credentials (KEY, TOKEN, SECRET, PASSWORD): the value stays in the golden's memory")
	allowExec := fs.Bool("allow-exec", false, "keep kling exec/cp working in the model's machines (debugging)")
	wait := fs.Duration("wait", 0, "how long to wait for the server to be ready, and again for the warm-up (default: by -model-bytes, at least 5m)")
	var envs, routes, tenants, allow, creds stringsFlag
	fs.Var(&envs, "e", "KEY=value for the server (repeatable); no secrets: the gateway adds credentials")
	fs.Var(&routes, "route", "\"METHOD /path\" forwarded under /v1/m/<model>/ (repeatable; required with -contract raw)")
	fs.Var(&tenants, "tenant", "tenant of ai.json this golden serves (repeatable; one golden per tenant unless -shared)")
	fs.Var(&allow, "allow", "domain the template and its replicas may reach (repeatable; egress allowlist)")
	fs.Var(&creds, "credential", "JSON file with a proxy credential for the template and its replicas ({\"domain\":…,\"env\":…,\"secret\":…,\"allow\":[…]}; mode 0600): the key stays on the host (repeatable)")
	if err := parseFlags(fs, reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *image == "" || *contract == "" {
		return errUso("usage: kling ai model deploy <name> -image I -contract C [-port P] [-health /path] [-warmup f.jsonl] [-cpus N] [-mem M] [-idle remove|pause|freeze]")
	}
	o := modelDeployOptions{
		Name: fs.Arg(0), Image: *image, Port: *port, Contract: *contract, Routes: routes,
		Health: aigw.HealthConfig{Path: *health, Expect: *expect},
		VCPUs:  *cpus, MemMiB: *mem, CPUPct: *cpuPct, Idle: *idle,
		MaxInflight: *maxInflight, MaxReplicas: *maxReplicas,
		Env: envs, AllowEnv: *allowEnv, AllowExec: *allowExec, Wait: *wait, Config: *cfgPath,
		ModelBytes: *modelBytes, Stall: *stall, WarmupKey: *warmupKey,
		Tenants: tenants, Alias: *alias, Shared: *shared, AllowDomains: allow,
	}
	for _, p := range creds {
		cr, err := leerCredencial(p)
		if err != nil {
			return err
		}
		o.Credentials = append(o.Credentials, cr)
	}
	if *healthTimeout > 0 {
		o.Health.Timeout = healthTimeout.String()
	}
	if *warmup != "" {
		f, err := os.Open(*warmup)
		if err != nil {
			return err
		}
		o.Warmup, err = dorado.ParseWarmup(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", *warmup, err)
		}
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	t0 := time.Now()
	r, err := modelDeploy(ctx, api.NewClient(hostOf(*host)), o)
	if err != nil {
		return err
	}
	fmt.Printf(uiMarks().ok+" %s  version %d of %s  (%s of memory, %s in total)\n",
		r.Snapshot, r.Version, o.Name, human(r.Golden.Snapshot.MemBytes), time.Since(t0).Round(time.Second))
	if r.Written {
		fmt.Printf("Wrote model %q to %s. Load it with:  kling ai reload\n", o.Name, o.Config)
		return nil
	}
	fmt.Printf("Model %q is already in %s: serve the new version with\n  kling ai model roll %s -to %d\n", o.Name, o.Config, o.Name, r.Version)
	return nil
}

// modelDeploy valida, hace el dorado de la versión siguiente y, si el modelo es
// nuevo, escribe su bloque en ai.json.
func modelDeploy(ctx context.Context, c *api.Client, o modelDeployOptions) (*modelDeployResult, error) {
	switch {
	case !deployNameRE.MatchString(o.Name):
		return nil, fmt.Errorf("model name %q: lowercase letters, digits, '.', '_' and '-', at most 56", o.Name)
	case o.Contract == aigw.ContractChispaClassify:
		return nil, errors.New("-contract chispa-classify is a classic Chispa replica: make it with kling ai chispa deploy")
	case !deployContracts[o.Contract]:
		return nil, fmt.Errorf("-contract %q: use openai, openai-embeddings, chispa-decide or raw", o.Contract)
	case o.Contract == aigw.ContractRaw && len(o.Routes) == 0:
		return nil, errors.New("-contract raw forwards only the declared routes: add at least one -route \"POST /path\"")
	case !deployIdle[o.Idle]:
		return nil, fmt.Errorf("-idle %q: use remove, pause or freeze", o.Idle)
	case o.MaxInflight < 0 || o.MaxReplicas < 0:
		return nil, errors.New("-max-inflight and -max-replicas cannot be negative")
	}
	var probeTimeout time.Duration
	if o.Health.Timeout != "" {
		d, err := time.ParseDuration(o.Health.Timeout)
		if err != nil {
			return nil, fmt.Errorf("-health-timeout: %w", err)
		}
		probeTimeout = d
	}

	// El registro antes que el dorado: descubrir al final que el nombre es de
	// otro tipo de modelo es tirar minutos de carga.
	reg, err := readRegistry(o.Config)
	if err != nil {
		return nil, err
	}
	existing := false
	idle := o.Idle
	if raw, ok := reg.models[o.Name]; ok {
		var b struct {
			Kind string `json:"kind"`
			Idle string `json:"idle"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("%s: model %q: %w", o.Config, o.Name, err)
		}
		if b.Kind != aigw.KindHTTP {
			return nil, fmt.Errorf("%s: model %q is kind %q, not an http backend: pick another name", o.Config, o.Name, b.Kind)
		}
		existing, idle = true, b.Idle
		if len(o.Tenants) > 0 || o.Alias != "" || o.Shared {
			return nil, fmt.Errorf("%s: model %q is already in the registry: -tenant, -alias and -shared only write a new block; edit its block instead", o.Config, o.Name)
		}
	}
	if err := reg.validarInquilinos(o); err != nil {
		return nil, err
	}
	labels := map[string]string{dorado.LabelModel: o.Name}
	// C9-15: las réplicas de un bloque idle remove (el defecto de kind http)
	// nunca se congelan en diferencial: el espejo del dorado en el almacén
	// sería escritura que nadie lee.
	if idle == "" || idle == aigw.IdleRemove {
		labels[api.LabelGoldenMirror] = api.GoldenMirrorOff
	}

	snaps, err := c.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(snaps))
	for _, s := range snaps {
		names = append(names, s.Name)
	}
	n := dorado.NextVersion(o.Name, names)
	snap := dorado.Versioned(o.Name, n)

	labels[dorado.LabelVersion] = strconv.Itoa(n)

	fmt.Printf("Making the golden snapshot %s (%d vCPU, %d MiB)...\n", snap, o.VCPUs, o.MemMiB)
	g, err := dorado.Make(ctx, c, dorado.Options{
		Image: o.Image, Snapshot: snap, Port: o.Port,
		VCPUs: o.VCPUs, MemMiB: o.MemMiB, CPUPct: o.CPUPct,
		Health: dorado.Probe{Path: o.Health.Path, Expect: o.Health.Expect, Timeout: probeTimeout},
		Warmup: o.Warmup, Env: o.Env, AllowEnv: o.AllowEnv, AllowExec: o.AllowExec,
		Labels:     labels,
		Wait:       o.Wait,
		ModelBytes: o.ModelBytes, Stall: o.Stall, WarmupKey: o.WarmupKey,
		AllowDomains: o.AllowDomains, Credentials: o.Credentials,
		Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	})
	if err != nil {
		return nil, err
	}
	precalentar(ctx, c, snap)
	res := &modelDeployResult{Snapshot: snap, Version: n, Golden: g}
	if existing {
		return res, nil
	}
	block := map[string]any{
		"kind": aigw.KindHTTP, "snapshot": snap, "port": o.Port, "contract": o.Contract,
	}
	if len(o.Routes) > 0 {
		block["routes"] = o.Routes
	}
	if o.Health != (aigw.HealthConfig{}) {
		block["health"] = o.Health
	}
	if o.Idle != "" {
		block["idle"] = o.Idle
	}
	if o.MaxInflight > 0 {
		block["max_inflight"] = o.MaxInflight
	}
	if o.MaxReplicas > 0 {
		block["max_replicas"] = o.MaxReplicas
	}
	if len(o.Tenants) > 0 {
		block["tenants"] = o.Tenants
	}
	if o.Alias != "" {
		block["alias"] = o.Alias
	}
	if o.Shared {
		block["shared"] = true
	}
	raw, err := json.Marshal(block)
	if err != nil {
		return nil, err
	}
	// Se relee: el dorado tarda minutos y alguien pudo editar el registro.
	if reg, err = readRegistry(o.Config); err != nil {
		return nil, err
	}
	if _, ok := reg.models[o.Name]; ok {
		return res, nil
	}
	reg.models[o.Name] = raw
	if err := reg.write(); err != nil {
		return nil, err
	}
	res.Written = true
	return res, nil
}

// validarInquilinos comprueba -tenant y -alias contra el registro ANTES de
// fabricar (P4): cada inquilino tiene que estar declarado en ai.json, una
// vez; el alias tiene forma de nombre de modelo y no es el de ningún modelo.
// Las reglas del bloque entero (un inquilino por chispa-decide, un inquilino
// por alias) las vuelve a aplicar el gateway al cargar.
func (r *registry) validarInquilinos(o modelDeployOptions) error {
	if len(o.Tenants) > 0 {
		var tops struct {
			Tenants []struct {
				Name string `json:"name"`
			} `json:"tenants"`
		}
		if raw, ok := r.top["tenants"]; ok {
			if err := json.Unmarshal(raw, &tops.Tenants); err != nil {
				return fmt.Errorf("%s: tenants: %w", r.path, err)
			}
		}
		declarados := map[string]bool{}
		for _, t := range tops.Tenants {
			declarados[t.Name] = true
		}
		vistos := map[string]bool{}
		for _, t := range o.Tenants {
			switch {
			case !declarados[t]:
				return fmt.Errorf("-tenant %s: no such tenant in %s (add it to \"tenants\" first)", t, r.path)
			case vistos[t]:
				return fmt.Errorf("-tenant %s given twice", t)
			}
			vistos[t] = true
		}
	}
	if o.Alias != "" {
		if !deployNameRE.MatchString(o.Alias) {
			return fmt.Errorf("-alias %q: lowercase letters, digits, '.', '_' and '-'", o.Alias)
		}
		if _, ok := r.models[o.Alias]; ok || o.Alias == o.Name {
			return fmt.Errorf("-alias %q is the name of a model: an alias must not hide one", o.Alias)
		}
	}
	return nil
}

// precalentar pide al daemon que traiga el dorado a la caché de página
// (C9-10), sin esperar ni anclar: la primera réplica no lo lee del disco. Un
// daemon que no sabe (anterior, o macOS) no es un fallo; otro error se avisa y
// se sigue, porque el dorado vale igual.
func precalentar(ctx context.Context, c *api.Client, snap string) {
	r, err := c.WarmSnapshot(ctx, snap, api.WarmSnapshotRequest{})
	switch {
	case err == nil:
		fmt.Printf("  warming %s into the page cache (%s of %s already there)\n", snap, human(r.CachedBytes), human(r.Bytes))
	case api.WarmUnavailable(err):
	default:
		fmt.Printf("  note: could not warm %s into the page cache: %v; the first replica reads it from disk\n", snap, err)
	}
}

// leerCredencial lee una credencial del proxy de un fichero JSON (-credential).
// La clave va en el fichero y no en una bandera (que se ve en ps y en el
// historial), y por eso el fichero solo lo puede leer su dueño.
func leerCredencial(path string) (api.CredentialSpec, error) {
	var cr api.CredentialSpec
	st, err := os.Stat(path)
	if err != nil {
		return cr, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return cr, fmt.Errorf("-credential %s: mode %v lets others read the key: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cr, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cr); err != nil {
		// Sin el texto del fichero en el error: lleva la clave.
		return api.CredentialSpec{}, fmt.Errorf("-credential %s: not a credential (JSON with domain, env, secret, allow…)", path)
	}
	return cr, nil
}

// registry es ai.json leído a medias: solo se entra en "models", y el resto del
// fichero (tareas, inquilinos, campos que este binario no conozca) se vuelve a
// escribir tal cual. No pasa por aigw.LoadConfig a propósito: aquella hace
// absolutas las rutas relativas y las escribiría así.
type registry struct {
	path   string
	orig   []byte // contenido leído; nil si no existía
	mode   fs.FileMode
	top    map[string]json.RawMessage
	models map[string]json.RawMessage
}

func readRegistry(path string) (*registry, error) {
	r := &registry{path: path, mode: 0o600, top: map[string]json.RawMessage{}, models: map[string]json.RawMessage{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil {
		r.mode = st.Mode().Perm()
	}
	r.orig = b
	if err := json.Unmarshal(b, &r.top); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m, ok := r.top["models"]; ok && !bytes.Equal(bytes.TrimSpace(m), []byte("null")) {
		if err := json.Unmarshal(m, &r.models); err != nil {
			return nil, fmt.Errorf("%s: models: %w", path, err)
		}
	}
	return r, nil
}

// write lo escribe entero, sangrado, por un temporal y un rename: un
// gateway que relea a la vez ve el fichero viejo o el nuevo, nunca medio.
func (r *registry) write() error {
	m, err := json.Marshal(r.models)
	if err != nil {
		return err
	}
	r.top["models"] = m
	b, err := json.MarshalIndent(r.top, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(b, '\n'), r.mode)
}

// restore deja el fichero como se leyó (o lo borra si no existía).
func (r *registry) restore() error {
	if r.orig == nil {
		return os.Remove(r.path)
	}
	return writeFileAtomic(r.path, r.orig, r.mode)
}

func writeFileAtomic(path string, b []byte, mode fs.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
