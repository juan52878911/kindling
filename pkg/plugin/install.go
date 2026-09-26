package plugin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ReleaseURL es de dónde baja `kling plugins install` por defecto: las
// releases del propio repo de kindling. Los assets se llaman
// kling-<nombre>-<os>-<arch> y cada release publica un único SHA256SUMS.
const ReleaseURL = "https://github.com/juan52878911/kindling/releases/download"

// Límites de lo que se lee de fuera. Un binario de extensión ronda las decenas
// de MiB; 512 MiB deja margen sin permitir que un servidor nos llene la memoria.
const (
	maxAssetBytes = 512 << 20
	maxSumsBytes  = 1 << 20
	maxSidecar    = 64 << 10
)

// InstallDir es donde `kling plugins install` deja las extensiones: el primer
// directorio de $KLING_PLUGIN_PATH; si no, $XDG_DATA_HOME/kling/plugins; si
// no, ~/.local/share/kling/plugins. Todos están en SearchPath, así que lo que
// se instala ahí se descubre sin tocar el PATH.
func InstallDir() (string, error) {
	if v := os.Getenv("KLING_PLUGIN_PATH"); v != "" {
		for _, d := range filepath.SplitList(v) {
			if d != "" {
				return d, nil
			}
		}
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "kling", "plugins"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the extensions directory: %v (set KLING_PLUGIN_PATH)", err)
	}
	return filepath.Join(h, ".local", "share", "kling", "plugins"), nil
}

// Sidecar es kling-<nombre>.json, junto al binario: de dónde salió y con qué
// hash. Es lo que `kling plugins ls` enseña y lo que `rm` usa para saber qué
// compañeros quitar. El descubrimiento no lo toma por extensión (tiene punto).
type Sidecar struct {
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	URL        string    `json:"url"`
	SHA256     string    `json:"sha256"`
	Installed  time.Time `json:"installed"`
	Companions []string  `json:"companions,omitempty"`
}

// SidecarPath es la ruta del kling-<n>.json de un binario de extensión.
func SidecarPath(bin string) string { return bin + ".json" }

// ReadSidecar lee el kling-<n>.json de un binario de extensión.
func ReadSidecar(bin string) (*Sidecar, error) { return readSidecar(SidecarPath(bin)) }

func readSidecar(p string) (*Sidecar, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s Sidecar
	if err := json.NewDecoder(io.LimitReader(f, maxSidecar)).Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	return &s, nil
}

// InstallOptions dice qué instalar y de dónde.
type InstallOptions struct {
	Name string
	// Tag es la release ("v0.13.0"). Vacío = "v"+CoreVersion.
	Tag string
	// From es la URL https de un asset; su SHA256SUMS se busca al lado.
	From string
	// File es un binario local. Si hay un SHA256SUMS en su directorio, se
	// verifica con él; los compañeros se buscan en el mismo directorio.
	File string
	// SHA256 es el hash esperado del binario principal. Con From permite
	// prescindir del SHA256SUMS (salvo para los compañeros).
	SHA256 string
	// Dir es el directorio de destino. Vacío = InstallDir().
	Dir string
	// CoreVersion es la versión de este kling, para la etiqueta por defecto y
	// para comprobar min_kling.
	CoreVersion string
	// GOOS y GOARCH eligen el asset. Vacíos = los de este binario.
	GOOS, GOARCH string
	// Client hace las descargas. nil = uno con plazo.
	Client *http.Client
	// ReleaseURL sustituye a la constante (tests). Vacío = ReleaseURL.
	ReleaseURL string

	// allowHTTP deja descargar por http:// en los tests del paquete. No se
	// puede activar desde fuera a propósito: fuera de un test, http es un
	// binario que cualquiera en la red puede cambiar por el camino.
	allowHTTP bool
}

// Installed es lo que se instaló.
type Installed struct {
	Path       string
	Manifest   *Manifest
	Sidecar    Sidecar
	Replaced   *Sidecar // lo que había antes, si era una actualización
	Companions []string // rutas de los compañeros instalados
}

// DevVersion dice si v no sirve como etiqueta de release: "dev", o lo que da
// `git describe` fuera de una etiqueta exacta ("v0.12.0-5-gabc", "-dirty").
func DevVersion(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if _, ok := parseVersion(v); !ok {
		return true
	}
	return strings.ContainsAny(v, "-+ ")
}

// source es de dónde sale cada fichero: una URL base (release o directorio
// del asset de -from) o un directorio local (-file).
type source struct {
	o       *InstallOptions
	client  *http.Client
	baseURL string // termina en "/"
	baseDir string
	sums    map[string]string // nombre de asset -> sha256
	sumsErr error
	loaded  bool
}

// Install baja, verifica y deja lista una extensión. Nada llega al directorio
// de extensiones sin haber pasado el sha256, y nada queda con su nombre final
// sin haber impreso un manifiesto válido.
func Install(ctx context.Context, o InstallOptions) (*Installed, error) {
	if !reName.MatchString(o.Name) {
		return nil, fmt.Errorf("invalid extension name %q", o.Name)
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.SHA256 != "" {
		o.SHA256 = strings.ToLower(strings.TrimSpace(o.SHA256))
		if b, err := hex.DecodeString(o.SHA256); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("-sha256 %q is not a sha256 in hex", o.SHA256)
		}
	}
	if o.From != "" && o.File != "" {
		return nil, errors.New("use -from or -file, not both")
	}
	if o.Dir == "" {
		d, err := InstallDir()
		if err != nil {
			return nil, err
		}
		o.Dir = d
	}

	src := &source{o: &o}
	base := o.Client
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Minute}
	}
	// Una redirección a http:// sería la misma puerta trasera que una URL
	// http: GitHub redirige los assets a otro dominio, pero siempre por https.
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return o.checkScheme(req.URL)
	}
	src.client = &c

	asset := assetName("kling-"+o.Name, o.GOOS, o.GOARCH)
	var mainURL string
	switch {
	case o.File != "":
		src.baseDir = filepath.Dir(o.File)
		mainURL = "file://" + absPath(o.File)
	case o.From != "":
		u, err := url.Parse(o.From)
		if err != nil {
			return nil, fmt.Errorf("-from: %v", err)
		}
		if err := o.checkScheme(u); err != nil {
			return nil, err
		}
		dir := *u
		dir.Path = strings.TrimSuffix(path.Dir(u.Path), "/") + "/"
		dir.RawPath, dir.RawQuery, dir.Fragment = "", "", ""
		src.baseURL = dir.String()
		asset = path.Base(u.Path)
		mainURL = u.String()
	default:
		tag := o.Tag
		if tag == "" {
			if DevVersion(o.CoreVersion) {
				return nil, fmt.Errorf("this kling (%s) is not a release build, so there is no release to match: "+
					"say which one (kling plugins install %s@v0.13.0), or use -from URL or -file PATH", o.CoreVersion, o.Name)
			}
			tag = "v" + strings.TrimPrefix(o.CoreVersion, "v")
		}
		if !strings.HasPrefix(tag, "v") || strings.ContainsAny(tag, "/?#% ") {
			return nil, fmt.Errorf("invalid release tag %q", tag)
		}
		rel := o.ReleaseURL
		if rel == "" {
			rel = ReleaseURL
		}
		src.baseURL = strings.TrimSuffix(rel, "/") + "/" + tag + "/"
		mainURL = src.baseURL + asset
	}

	body, sum, err := src.fetch(ctx, asset, o.File, o.SHA256)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	final := filepath.Join(o.Dir, "kling-"+o.Name)
	tmp, err := writeTemp(o.Dir, ".kling-"+o.Name, body)
	if err != nil {
		return nil, err
	}
	cleanup := []string{tmp}
	defer func() {
		for _, p := range cleanup {
			os.Remove(p)
		}
	}()

	m, err := loadManifest(ctx, tmp)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid kling extension: %v", asset, err)
	}
	if m.Name != o.Name {
		return nil, fmt.Errorf("%s says it is extension %q, not %q", asset, m.Name, o.Name)
	}
	if !VersionAtLeast(o.CoreVersion, m.MinKling) {
		return nil, fmt.Errorf("%s %s needs kling %s or newer (this is %s)", o.Name, m.Version, m.MinKling, o.CoreVersion)
	}

	// Los compañeros se bajan y verifican todos antes de mover nada a su
	// sitio: una instalación a medias sería una extensión que falla al usarse.
	type pending struct{ tmp, final string }
	var comps []pending
	for _, cname := range m.Companions {
		var local string
		if o.File != "" {
			local = firstExisting(filepath.Join(src.baseDir, assetName(cname, o.GOOS, o.GOARCH)), filepath.Join(src.baseDir, cname))
			if local == "" {
				return nil, fmt.Errorf("%s needs %s next to it: not found in %s", o.Name, cname, src.baseDir)
			}
		}
		cbody, _, err := src.fetch(ctx, assetName(cname, o.GOOS, o.GOARCH), local, "")
		if err != nil {
			return nil, fmt.Errorf("companion %s: %w", cname, err)
		}
		ctmp, err := writeTemp(o.Dir, "."+cname, cbody)
		if err != nil {
			return nil, err
		}
		cleanup = append(cleanup, ctmp)
		comps = append(comps, pending{ctmp, filepath.Join(o.Dir, cname)})
	}

	res := &Installed{Path: final, Manifest: m}
	if old, err := ReadSidecar(final); err == nil {
		res.Replaced = old
	}
	for _, c := range comps {
		if err := os.Rename(c.tmp, c.final); err != nil {
			return nil, err
		}
		res.Companions = append(res.Companions, c.final)
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	res.Sidecar = Sidecar{
		Name: o.Name, Version: m.Version, URL: mainURL, SHA256: sum,
		Installed: time.Now().UTC().Truncate(time.Second), Companions: m.Companions,
	}
	b, _ := json.MarshalIndent(res.Sidecar, "", "  ")
	if err := os.WriteFile(SidecarPath(final), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

func (o *InstallOptions) checkScheme(u *url.URL) error {
	if u.Scheme == "https" || (o.allowHTTP && u.Scheme == "http") {
		return nil
	}
	return fmt.Errorf("refusing %s: extensions are only downloaded over https", u.Redacted())
}

func assetName(bin, goos, goarch string) string { return bin + "-" + goos + "-" + goarch }

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// fetch lee un asset (de la URL base o, si local no está vacío, de disco) y
// lo verifica: contra want si se dio, y contra SHA256SUMS si lo lista. Un
// asset remoto que no se puede verificar de ninguna de las dos formas no se
// acepta; uno local sí, porque ya está en la máquina de quien lo instala.
func (s *source) fetch(ctx context.Context, asset, local, want string) ([]byte, string, error) {
	var body []byte
	var err error
	if local != "" {
		body, err = readFileLimited(local, maxAssetBytes)
	} else {
		body, err = s.get(ctx, s.baseURL+asset, maxAssetBytes)
	}
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(body)
	got := hex.EncodeToString(h[:])

	sums, sumsErr := s.loadSums(ctx)
	listed, inSums := sums[asset]
	if local != "" && !inSums {
		// Un SHA256SUMS local puede listar el nombre de asset o el del fichero.
		listed, inSums = sums[filepath.Base(local)]
	}
	switch {
	case want != "" && got != want:
		return nil, "", fmt.Errorf("%s: sha256 mismatch: got %s, want %s", asset, got, want)
	case inSums && got != listed:
		return nil, "", fmt.Errorf("%s: sha256 mismatch with SHA256SUMS: got %s, listed %s", asset, got, listed)
	case want == "" && !inSums && local == "":
		if sumsErr != nil {
			return nil, "", fmt.Errorf("%s: cannot verify it: %v (pass -sha256 to check it by hand)", asset, sumsErr)
		}
		return nil, "", fmt.Errorf("%s: not listed in SHA256SUMS, refusing to install it unverified", asset)
	}
	return body, got, nil
}

// loadSums baja (o lee) SHA256SUMS una sola vez. Que falte en remoto es un
// error que decide fetch; que falte en local es normal.
func (s *source) loadSums(ctx context.Context) (map[string]string, error) {
	if s.loaded {
		return s.sums, s.sumsErr
	}
	s.loaded = true
	s.sums = map[string]string{}
	var b []byte
	var err error
	if s.baseDir != "" {
		b, err = readFileLimited(filepath.Join(s.baseDir, "SHA256SUMS"), maxSumsBytes)
		if os.IsNotExist(err) {
			return s.sums, nil
		}
	} else {
		b, err = s.get(ctx, s.baseURL+"SHA256SUMS", maxSumsBytes)
	}
	if err != nil {
		s.sumsErr = fmt.Errorf("SHA256SUMS: %v", err)
		return s.sums, s.sumsErr
	}
	s.sums = ParseSums(b)
	return s.sums, nil
}

// ParseSums lee el formato de sha256sum: "<hex>  <nombre>" o "<hex> *<nombre>".
// Las líneas que no encajan se ignoran.
func ParseSums(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || len(f[0]) != 2*sha256.Size {
			continue
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			continue
		}
		out[path.Base(strings.TrimPrefix(f[1], "*"))] = strings.ToLower(f[0])
	}
	return out
}

func (s *source) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if err := s.o.checkScheme(pu); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", pu.Redacted(), resp.Status)
	}
	return readLimited(resp.Body, limit, pu.Redacted())
}

func readFileLimited(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, limit, p)
}

// readLimited lee hasta limit bytes y falla si hay más, en vez de truncar: un
// binario truncado tendría otro hash y el error despistaría.
func readLimited(r io.Reader, limit int64, what string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d MiB", what, limit>>20)
	}
	return b, nil
}

// writeTemp escribe body en un fichero oculto de dir (el descubrimiento no
// mira lo que no empieza por "kling-") con permiso de ejecución.
func writeTemp(dir, prefix string, body []byte) (string, error) {
	f, err := os.CreateTemp(dir, prefix+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, 0o755); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// Remove quita la extensión name de dir: el binario, su kling-<n>.json y los
// compañeros que no use otra extensión de dir. Solo toca dir: una extensión
// que está en otro sitio del PATH la puso otra cosa (un paquete, a mano) y no
// le toca a kling borrarla.
func Remove(dir, name string) (removed []string, err error) {
	if !reName.MatchString(name) {
		return nil, fmt.Errorf("invalid extension name %q", name)
	}
	bin := filepath.Join(dir, "kling-"+name)
	if _, err := os.Lstat(bin); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		for _, d := range SearchPath() {
			if p := filepath.Join(d, "kling-"+name); isExecutable(p) {
				return nil, fmt.Errorf("kling-%s is at %s, outside the extensions directory (%s): remove it the way it was installed", name, p, dir)
			}
		}
		return nil, fmt.Errorf("extension %q is not installed in %s", name, dir)
	}
	sc, _ := ReadSidecar(bin)
	if err := os.Remove(bin); err != nil {
		return nil, err
	}
	removed = append(removed, bin)
	if err := os.Remove(SidecarPath(bin)); err == nil {
		removed = append(removed, SidecarPath(bin))
	}
	if sc == nil {
		return removed, nil
	}
	// Con el .json propio ya borrado, lo que quede en companionsIn es de otras.
	inUse := companionsIn(dir)
	for _, c := range sc.Companions {
		if !reCompanion.MatchString(c) || inUse[c] {
			continue
		}
		p := filepath.Join(dir, c)
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		}
	}
	return removed, nil
}

// FileSHA256 es el sha256 de un fichero, para comparar el binario instalado
// con el que dice su kling-<n>.json.
func FileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxAssetBytes+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
