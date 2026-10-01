package android

import (
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Spec es el spec del constructor android (el "spec" de POST /images).
//
// El constructor no sabe qué lanzador lleva la imagen: el lanzador (hoy
// android-launch.sh o kling-phoned), la sonda de listo, los ganchos tras
// restaurar, uidump y demás llegan como ficheros (Files) y Service dice cuál
// arranca el entrypoint. Lo que sí genera él: el agente de invitado, el
// /entrypoint, entrypoint.args (de la configuración de la imagen OCI),
// android.conf (Conf), IMAGE.txt, el data.ext4 vacío y la base.
type Spec struct {
	// Arch es arm64 o amd64 (por defecto, la del host del daemon).
	Arch string `json:"arch,omitempty"`
	// Redroid cambia la imagen fijada (repo@digest del manifiesto de la
	// arquitectura, y el digest de su única capa si se quiere exigir).
	Redroid *RedroidSource `json:"redroid,omitempty"`
	// Service es la ruta, dentro de la imagen, de lo que el entrypoint
	// arranca y relanza antes de ceder el PID 1 al agente.
	Service string `json:"service"`
	// Env son variables KEY=VALUE que exporta el entrypoint (desde /etc/kling/env, 0600).
	Env []string `json:"env,omitempty"`
	// Files son los ficheros que se añaden a la imagen (en la capa).
	Files []File `json:"files,omitempty"`
	// Conf son las líneas de android.conf (se añaden a las de por defecto).
	Conf map[string]string `json:"conf,omitempty"`
	// InitArgs sustituye a los argumentos de /init que salen del
	// ENTRYPOINT+CMD de la imagen OCI (entrypoint.args).
	InitArgs []string `json:"init_args,omitempty"`
	// DataExt4MiB crea /usr/local/lib/kindling-android/data.ext4, un ext4
	// vacío y disperso de ese tamaño (DATA_MODE=tmpfs).
	DataExt4MiB int `json:"data_ext4_mib,omitempty"`
	// Slim adelgaza el rootfs de Android (lo de image/slim/apply.sh).
	Slim *Slim `json:"slim,omitempty"`
	// Verity pone la capa detrás de dm-verity (por defecto sí) con FECRoots
	// raíces de Reed-Solomon (por defecto 2; 0 = sin FEC).
	Verity   *bool `json:"verity,omitempty"`
	FECRoots *int  `json:"fec_roots,omitempty"`
	// BaseName es cómo se llama la base que se escribe (por defecto
	// "<nombre>-base", o la base de la petición). Con verity la base lleva la
	// tabla de ESTA capa: es suya.
	BaseName string `json:"base_name,omitempty"`
	// Packages sustituye a la lista de .deb fijados que van en la base
	// (subconjunto de debian_lock.go, por nombre; vacío = todos).
	Packages []string `json:"packages,omitempty"`
	// ARMTranslation es la traducción ARM de una imagen amd64 (issue #93,
	// prototypes/android/docs/traduccion-arm.md): "none" (por defecto) quita
	// el puente nativo que trae Redroid y deja las ABIs en x86_64; "libndk"
	// pone el libndk_translation de la imagen del emulador de Google (fijada
	// por URL y sha256, la baja el constructor); "redroid" deja el que trae
	// Redroid tal cual. En arm64 no hay nada que traducir: "" o "none".
	ARMTranslation string `json:"arm_translation,omitempty"`
}

// RedroidSource es una imagen de Redroid distinta de la fijada.
type RedroidSource struct {
	Repo   string `json:"repo"`
	Digest string `json:"digest"`
	Layer  string `json:"layer,omitempty"`
}

// File es un fichero de la imagen. Uno de Src (ruta en el host del daemon),
// Content o ContentBase64.
type File struct {
	Path          string  `json:"path"`
	Src           string  `json:"src,omitempty"`
	Content       *string `json:"content,omitempty"`
	ContentBase64 string  `json:"content_base64,omitempty"`
	// Mode en octal ("0755"); por defecto 0644 (0755 si Src es ejecutable).
	Mode string `json:"mode,omitempty"`
	UID  int    `json:"uid,omitempty"`
	GID  int    `json:"gid,omitempty"`
	// SHA256 exige ese hash del contenido.
	SHA256 string `json:"sha256,omitempty"`
}

// Slim es lo de prototypes/android/image/slim: propiedades que se añaden a
// vendor/build.prop, servicios de init que se comentan, apps del sistema que
// se quitan y un features.xml con las funciones que se declaran ausentes.
type Slim struct {
	Prop string `json:"prop,omitempty"`
	// PropName es de dónde salió Prop, para la marca que se deja en
	// build.prop (apply.sh pone la ruta de slim.prop).
	PropName    string   `json:"prop_name,omitempty"`
	Services    []string `json:"services,omitempty"`
	Apps        []string `json:"apps,omitempty"`
	FeaturesXML string   `json:"features_xml,omitempty"`
}

var (
	reName    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	reDigest  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reHex64   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reRepo    = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,200}$`)
	reEnv     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^\x00\r\n]*$`)
	reConfKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	reSvc     = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	reArg     = regexp.MustCompile(`^[^\x00\r\n]{1,256}$`)
)

// DefaultConf es el android.conf de build-image.sh con sus valores por
// defecto (Conf los cambia o añade otros).
var DefaultConf = map[string]string{
	"ANDROID_ROOT":       "/android",
	"ANDROID_WIDTH":      "720",
	"ANDROID_HEIGHT":     "1280",
	"ANDROID_DPI":        "320",
	"ANDROID_FPS":        "15",
	"ANDROID_DATA_MODE":  "overlay",
	"ANDROID_DATA_SIZE":  "2G",
	"ANDROID_NET":        "veth",
	"ANDROID_EXTRA_ARGS": "androidboot.use_redroid_stream=1",
}

const (
	libDir       = "/usr/local/lib/kindling-android"
	maxInline    = 1 << 20
	maxFiles     = 256
	maxDataExt4  = 64 << 10 // MiB
	defaultFECRS = 2
)

func (s *Spec) verity() bool { return s.Verity == nil || *s.Verity }

func (s *Spec) fecRoots() int {
	if !s.verity() {
		return 0
	}
	if s.FECRoots == nil {
		return defaultFECRS
	}
	return *s.FECRoots
}

// Validate comprueba el spec antes de tocar nada.
func (s *Spec) Validate() error {
	if s.Arch != "arm64" && s.Arch != "amd64" {
		return fmt.Errorf("arch must be arm64 or amd64, not %q", s.Arch)
	}
	if r := s.Redroid; r != nil {
		if !reRepo.MatchString(r.Repo) || strings.Contains(r.Repo, "..") {
			return fmt.Errorf("invalid redroid repo %q", r.Repo)
		}
		if !reDigest.MatchString(r.Digest) || (r.Layer != "" && !reDigest.MatchString(r.Layer)) {
			return fmt.Errorf("redroid digest and layer must be sha256:<64 hex>")
		}
	}
	if err := checkImagePath(s.Service); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	for _, kv := range s.Env {
		if !reEnv.MatchString(kv) {
			return fmt.Errorf("invalid env entry %q", kv)
		}
	}
	if len(s.Files) > maxFiles {
		return fmt.Errorf("too many files (%d, max %d)", len(s.Files), maxFiles)
	}
	seen := map[string]bool{}
	for _, f := range s.Files {
		if err := checkImagePath(f.Path); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		p := path.Clean(f.Path)
		if seen[p] {
			return fmt.Errorf("file %s listed twice", p)
		}
		seen[p] = true
		n := 0
		if f.Src != "" {
			n++
			if !path.IsAbs(f.Src) {
				return fmt.Errorf("file %s: src must be an absolute path", p)
			}
		}
		if f.Content != nil {
			n++
			if len(*f.Content) > maxInline {
				return fmt.Errorf("file %s: inline content over %d bytes: use src", p, maxInline)
			}
		}
		if f.ContentBase64 != "" {
			n++
			if base64.StdEncoding.DecodedLen(len(f.ContentBase64)) > maxInline {
				return fmt.Errorf("file %s: inline content over %d bytes: use src", p, maxInline)
			}
			if _, err := base64.StdEncoding.DecodeString(f.ContentBase64); err != nil {
				return fmt.Errorf("file %s: content_base64: %w", p, err)
			}
		}
		if n != 1 {
			return fmt.Errorf("file %s: give exactly one of src, content, content_base64", p)
		}
		if f.Mode != "" {
			m, err := strconv.ParseUint(f.Mode, 8, 32)
			if err != nil || m > 0o7777 {
				return fmt.Errorf("file %s: invalid mode %q", p, f.Mode)
			}
		}
		if f.UID < 0 || f.GID < 0 || f.UID > 1<<31 || f.GID > 1<<31 {
			return fmt.Errorf("file %s: invalid owner", p)
		}
		if f.SHA256 != "" && !reHex64.MatchString(f.SHA256) {
			return fmt.Errorf("file %s: sha256 must be 64 hex", p)
		}
	}
	for k, v := range s.Conf {
		if !reConfKey.MatchString(k) || strings.ContainsAny(v, "\x00\r\n\"`$\\") {
			return fmt.Errorf("invalid conf entry %s=%q", k, v)
		}
	}
	for _, a := range s.InitArgs {
		if !reArg.MatchString(a) {
			return fmt.Errorf("invalid init arg %q", a)
		}
	}
	if s.DataExt4MiB < 0 || s.DataExt4MiB > maxDataExt4 {
		return fmt.Errorf("data_ext4_mib out of range")
	}
	if sl := s.Slim; sl != nil {
		for _, svc := range sl.Services {
			if !reSvc.MatchString(svc) {
				return fmt.Errorf("invalid slim service %q", svc)
			}
		}
		for _, a := range sl.Apps {
			if err := checkImagePath(a); err != nil {
				return fmt.Errorf("slim app %q: %w", a, err)
			}
		}
		if strings.ContainsAny(sl.PropName, "\n\r\x00") || len(sl.PropName) > 256 {
			return fmt.Errorf("invalid slim prop_name")
		}
		if len(sl.Prop) > maxInline || len(sl.FeaturesXML) > maxInline {
			return fmt.Errorf("slim prop or features too large")
		}
	}
	if s.FECRoots != nil && *s.FECRoots != 0 && (*s.FECRoots < 2 || *s.FECRoots > 24) {
		return fmt.Errorf("fec_roots must be 0 or 2..24")
	}
	if s.BaseName != "" && !reName.MatchString(s.BaseName) {
		return fmt.Errorf("invalid base_name %q", s.BaseName)
	}
	switch s.ARMTranslation {
	case "", TranslationNone:
	case TranslationLibndk, TranslationRedroid:
		if s.Arch != "amd64" {
			return fmt.Errorf("arm_translation %q is for amd64 images: arm64 runs ARM apps natively", s.ARMTranslation)
		}
	default:
		return fmt.Errorf("arm_translation must be none, libndk or redroid, not %q", s.ARMTranslation)
	}
	lock := debianLock[s.Arch]
	for _, p := range s.Packages {
		found := false
		for _, d := range lock.Packages {
			found = found || d.Name == p
		}
		if !found {
			return fmt.Errorf("package %q is not in the pinned set (internal/android/debian_lock.go)", p)
		}
	}
	return nil
}

func checkImagePath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("must be a clean absolute path inside the image")
	}
	for _, c := range strings.Split(p[1:], "/") {
		if c == "" || c == "." || c == ".." || len(c) > 255 {
			return fmt.Errorf("invalid path component %q", c)
		}
	}
	for _, bad := range []string{"/proc", "/sys", "/dev", "/upper"} {
		if p == bad || strings.HasPrefix(p, bad+"/") {
			return fmt.Errorf("%s is not allowed", bad)
		}
	}
	return nil
}

// confText escribe android.conf (lo leen los lanzadores con "source").
func (s *Spec) confText() string {
	m := map[string]string{}
	for k, v := range DefaultConf {
		m[k] = v
	}
	for k, v := range s.Conf {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Generado por el constructor android de kindling. Lo leen el lanzador\n# y android-sh (bash, \"source\").\n")
	for _, k := range keys {
		v := m[k]
		if strings.ContainsAny(v, " \t'") || v == "" {
			v = `"` + v + `"`
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.String()
}
