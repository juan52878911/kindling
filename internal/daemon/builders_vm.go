package daemon

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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/lazyre"
)

// APLANAR DENTRO DE UNA MICROVM (image import -isolate).
//
// El tar de una capa es lo más hostil que entra al host. Con isolate el
// constructor oci se parte en dos:
//
//  1. fase "pull" (como siempre, con el usuario de construcción): baja y
//     verifica por digest los blobs y deja isolate.json con la lista. No abre
//     ningún tar.
//  2. el daemon vuelve a comprobar cada blob por sha256 (sin seguir enlaces:
//     el directorio de trabajo es de un proceso que parsea JSON de un
//     registro), enciende una microVM de la imagen de herramientas
//     (kling-build-tools, que lleva este mismo binario) sin red, le pasa los
//     blobs, el request.json y el agente, y corre allí `kling builder oci` en
//     fase "flatten": sin red, con el digest fijado, desde sus blobs.
//  3. del invitado solo vuelven el ext4 y el recipe.json, con tope de tamaño:
//     el disco escribible de la VM (DiskMiB) y el límite al descargar. El
//     ext4 pasa luego por adoptarSalida como el de cualquier constructor.
//
// Los ficheros viajan por el /files del agente (64 MiB de subida, 256 de
// bajada por petición): lo grande va en trozos que el invitado junta o parte
// con cat y split.

// ToolsImage es la imagen de herramientas (kling image build-tools).
const ToolsImage = "kling-build-tools"

const (
	vmWork      = "/work" // directorio de trabajo dentro de la VM
	vmBin       = "/usr/local/bin/kling"
	vmMaxRecipe = 1 << 20   // tope del recipe.json que sale de la VM
	vmMaxPlan   = 256 << 10 // tope del isolate.json que deja la fase pull
)

// vmTrozo es el tamaño de un trozo en cada sentido (variable para los tests).
var vmTrozo int64 = 32 << 20

var reBlobDigest = lazyre.New(`^sha256:[0-9a-f]{64}$`)

// planAislado es el isolate.json de la fase pull (cmd/kling IsolatePlan).
type planAislado struct {
	Digest string   `json:"digest"`
	Epoch  int64    `json:"epoch"`
	Blobs  []string `json:"blobs"`
}

// aislarSpec dice si la petición pide aplanar en una VM.
func aislarSpec(req api.BuildImageRequest) bool {
	if req.Builder != "oci" || len(req.Spec) == 0 {
		return false
	}
	var s struct {
		Isolate bool `json:"isolate"`
		Arch    string
	}
	return json.Unmarshal(req.Spec, &s) == nil && s.Isolate
}

// leerPlanAislado lee y valida el isolate.json: lo escribe un proceso del
// usuario de construcción, así que sin enlaces, con tope y con cada digest
// comprobado antes de formar una ruta con él.
func leerPlanAislado(work string) (planAislado, error) {
	var p planAislado
	f, err := os.OpenFile(filepath.Join(work, "isolate.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return p, fmt.Errorf("the pull phase left no plan: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return p, errors.New("isolate.json is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, vmMaxPlan+1))
	if err != nil || len(b) > vmMaxPlan {
		return p, errors.New("isolate.json is unreadable or too big")
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("isolate.json: %w", err)
	}
	if !reBlobDigest.MatchString(p.Digest) {
		return p, fmt.Errorf("isolate.json: bad digest %q", p.Digest)
	}
	if len(p.Blobs) == 0 || len(p.Blobs) > 4096 {
		return p, fmt.Errorf("isolate.json: %d blobs", len(p.Blobs))
	}
	for _, d := range p.Blobs {
		if !reBlobDigest.MatchString(d) {
			return p, fmt.Errorf("isolate.json: bad blob digest %q", d)
		}
	}
	return p, nil
}

// blobVerificado abre el blob de digest en la primera de las cachés donde
// esté como fichero regular y con ese sha256. Devuelve el fichero, posicionado
// al principio, y su tamaño.
func blobVerificado(cachesOCI []string, digest string) (*os.File, int64, error) {
	hexs := strings.TrimPrefix(digest, "sha256:")
	var last error = fmt.Errorf("blob %s is in no cache", digest)
	for _, c := range cachesOCI {
		f, err := os.OpenFile(filepath.Join(c, "sha256", hexs), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			f.Close()
			last = fmt.Errorf("blob %s: not a regular file", digest)
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			last = err
			continue
		}
		if hex.EncodeToString(h.Sum(nil)) != hexs {
			f.Close()
			last = fmt.Errorf("blob %s: content does not match its digest", digest)
			continue
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			last = err
			continue
		}
		return f, fi.Size(), nil
	}
	return nil, 0, last
}

// invitadoVM es lo que el aplanado necesita de la VM (en los tests, un falso).
type invitadoVM interface {
	Put(ctx context.Context, path string, mode os.FileMode, r io.Reader) error
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Size(ctx context.Context, path string) (int64, error)
	Exec(ctx context.Context, cmd []string) (exit int, out string, err error)
}

// subirFichero copia r (size bytes) a path en el invitado; lo que pasa de un
// trozo va en trozos que el invitado junta con cat.
func subirFichero(ctx context.Context, g invitadoVM, path string, mode os.FileMode, r io.Reader, size int64) error {
	if size <= vmTrozo {
		return g.Put(ctx, path, mode, r)
	}
	n := 0
	for left := size; left > 0; n++ {
		l := min(left, vmTrozo)
		if err := g.Put(ctx, fmt.Sprintf("%s.part%05d", path, n), 0o600, io.LimitReader(r, l)); err != nil {
			return err
		}
		left -= l
	}
	cmd := fmt.Sprintf(`set -e; for f in %[1]s.part*; do cat "$f" >> %[1]s; rm "$f"; done; chmod %04o %[1]s`, shQ(path), mode.Perm())
	if exit, out, err := g.Exec(ctx, []string{"sh", "-c", cmd}); err != nil || exit != 0 {
		return fmt.Errorf("joining %s inside the microVM: exit %d %v %s", path, exit, err, strings.TrimSpace(out))
	}
	return nil
}

// bajarFichero copia path del invitado a w, sin pasar de max bytes: el
// invitado no es de fiar, y el tope se comprueba antes de bajar nada.
func bajarFichero(ctx context.Context, g invitadoVM, path string, w io.Writer, max int64) (int64, error) {
	size, err := g.Size(ctx, path)
	if err != nil {
		return 0, err
	}
	if size > max {
		return 0, fmt.Errorf("%s is %d MiB, over the limit of %d MiB", path, size>>20, max>>20)
	}
	if size <= vmTrozo {
		return copiarDe(ctx, g, path, w, size)
	}
	// split -b trozos de vmTrozo con sufijos aa, ab...; se borra el entero
	// para no gastar doble disco mientras se baja.
	cmd := fmt.Sprintf(`set -e; split -b %d %[2]s %[2]s.p.; rm %[2]s`, vmTrozo, shQ(path))
	if exit, out, err := g.Exec(ctx, []string{"sh", "-c", cmd}); err != nil || exit != 0 {
		return 0, fmt.Errorf("splitting %s inside the microVM: exit %d %v %s", path, exit, err, strings.TrimSpace(out))
	}
	var total int64
	for i := int64(0); i*vmTrozo < size || i == 0; i++ {
		suf := string(rune('a'+i/26)) + string(rune('a'+i%26))
		part := path + ".p." + suf
		want := min(size-i*vmTrozo, vmTrozo)
		n, err := copiarDe(ctx, g, part, w, want)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// copiarDe baja exactamente want bytes: ni uno más ni uno menos.
func copiarDe(ctx context.Context, g invitadoVM, path string, w io.Writer, want int64) (int64, error) {
	rc, err := g.Get(ctx, path)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(w, io.LimitReader(rc, want+1))
	if err != nil {
		return n, err
	}
	if n != want {
		return n, fmt.Errorf("%s: got %d bytes, expected %d", path, n, want)
	}
	return n, nil
}

// shQ entrecomilla una ruta para sh.
func shQ(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// cargaVM es todo lo que se manda a la VM y lo que se espera de vuelta.
type cargaVM struct {
	Name     string
	Request  []byte // request.json
	Agent    string // ruta en el host del agente del invitado
	Plan     planAislado
	Caches   []string // cachés OCI donde buscar los blobs
	Epoch    int64
	MaxImage int64                         // tope del ext4 que vuelve
	Log      func(format string, a ...any) // tiempos de cada paso (puede ser nil)
}

// aplanarEnInvitado corre el protocolo entero contra g y deja el ext4 en
// w y el recipe.json en recipe (nil = se descarta).
func aplanarEnInvitado(ctx context.Context, g invitadoVM, c cargaVM, ext4 io.Writer, recipe io.Writer) (string, error) {
	t0 := time.Now()
	logf := func(format string, a ...any) {
		if c.Log != nil {
			c.Log(format, a...)
		}
	}
	cache := vmWork + "/cache/oci/sha256/"
	for _, d := range c.Plan.Blobs {
		f, size, err := blobVerificado(c.Caches, d)
		if err != nil {
			return "", err
		}
		err = subirFichero(ctx, g, cache+strings.TrimPrefix(d, "sha256:"), 0o644, f, size)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("sending blob %s: %w", d, err)
		}
	}
	if err := g.Put(ctx, vmWork+"/request.json", 0o600, bytes.NewReader(c.Request)); err != nil {
		return "", err
	}
	if c.Agent != "" {
		fa, err := os.Open(c.Agent)
		if err != nil {
			return "", fmt.Errorf("guest agent: %w", err)
		}
		fi, _ := fa.Stat()
		err = subirFichero(ctx, g, vmWork+"/kling-guest", 0o755, fa, fi.Size())
		fa.Close()
		if err != nil {
			return "", err
		}
	}
	env := []string{"env", "-i", "PATH=/usr/local/bin:/usr/bin:/bin",
		"KLING_ROOT=" + vmWork, "KLING_LIB_DIR=" + vmWork, "KLING_BUILD_DIR=" + vmWork,
		"KLING_CACHE_DIR=" + vmWork + "/cache", "KLING_OUT_DIR=" + vmWork + "/out",
		"KLING_IMAGE_NAME=" + c.Name, "KLING_OCI_NO_REUSE=1",
		"KLING_OCI_PHASE=flatten", "KLING_OCI_PINNED=" + c.Plan.Digest,
		"SOURCE_DATE_EPOCH=" + strconv.FormatInt(c.Plan.Epoch, 10),
		vmBin, "builder", "oci", vmWork}
	logf("blobs and request sent in %.1f s", time.Since(t0).Seconds())
	t0 = time.Now()
	exit, out, err := g.Exec(ctx, env)
	if err != nil {
		return out, fmt.Errorf("running the builder inside the microVM: %w", err)
	}
	if exit != 0 {
		return out, fmt.Errorf("the builder failed inside the microVM (exit %d):\n%s", exit, strings.TrimSpace(out))
	}
	logf("builder ran %.1f s inside", time.Since(t0).Seconds())
	t0 = time.Now()
	if _, err := bajarFichero(ctx, g, vmWork+"/out/"+c.Name+".ext4", ext4, c.MaxImage); err != nil {
		return out, fmt.Errorf("taking the image out of the microVM: %w", err)
	}
	if recipe != nil {
		if _, err := bajarFichero(ctx, g, vmWork+"/recipe.json", recipe, vmMaxRecipe); err != nil {
			return out, fmt.Errorf("taking recipe.json out of the microVM: %w", err)
		}
	}
	logf("image out of the microVM in %.1f s", time.Since(t0).Seconds())
	return out, nil
}

// ── la VM de verdad ─────────────────────────────────────────────────────────

// invitadoHTTP habla con el agente de la VM (/files y /exec/stream).
type invitadoHTTP struct {
	base string
	cli  *http.Client
}

func (g invitadoHTTP) filesURL(path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("path", path)
	return g.base + "/files?" + q.Encode()
}

func (g invitadoHTTP) Put(ctx context.Context, path string, mode os.FileMode, r io.Reader) error {
	q := url.Values{"mkdir": {"1"}, "mode": {"0" + strconv.FormatUint(uint64(mode.Perm()), 8)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, g.filesURL(path, q), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := g.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("guest PUT %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (g invitadoHTTP) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.filesURL(path, nil), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.cli.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("guest GET %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

func (g invitadoHTTP) Size(ctx context.Context, path string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.filesURL(path, url.Values{"stat": {"1"}}), nil)
	if err != nil {
		return 0, err
	}
	resp, err := g.cli.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("guest stat %s: %s", path, resp.Status)
	}
	var st api.FileStat
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return 0, err
	}
	if st.IsDir || st.Size < 0 {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	return st.Size, nil
}

// Exec corre cmd con el flujo NDJSON /exec/stream y se queda con los últimos
// 64 KiB de la salida (el error de un constructor sale al final).
func (g invitadoHTTP) Exec(ctx context.Context, cmd []string) (int, string, error) {
	body, _ := json.Marshal(api.ExecRequest{Cmd: cmd, TimeoutSeconds: int(buildTimeout / time.Second)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/exec/stream", bytes.NewReader(body))
	if err != nil {
		return -1, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.cli.Do(req)
	if err != nil {
		return -1, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return -1, "", fmt.Errorf("guest agent: %s", strings.TrimSpace(string(b)))
	}
	const cola = 64 << 10
	var out []byte
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev api.ExecEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Error != "":
			return -1, string(out), errors.New(ev.Error)
		case ev.Exit != nil:
			return *ev.Exit, string(out), nil
		case len(ev.Data) > 0:
			out = append(out, ev.Data...)
			if len(out) > cola {
				out = out[len(out)-cola:]
			}
		}
	}
	return -1, string(out), errors.New("the guest stopped answering before the command finished")
}

// aplanarEnVM es la fase 2 y 3 de buildWithBuilder: enciende la VM de
// herramientas, corre el protocolo y deja el ext4 y el recipe.json donde los
// habría dejado el constructor (out/ y work/), del usuario u si lo hay.
// salida es el registro de la fase pull; se le añade el de la VM.
func (s *Server) aplanarEnVM(ctx context.Context, req api.BuildImageRequest, work, out string,
	u *usuarioConstructor, caches []string, salida *bytes.Buffer) error {
	plan, err := leerPlanAislado(work)
	if err != nil {
		return err
	}
	var spec struct {
		MaxMB int `json:"max_mb"`
	}
	_ = json.Unmarshal(req.Spec, &spec)
	maxMB := spec.MaxMB
	if maxMB <= 0 {
		maxMB = 4096
	}
	rb, err := os.ReadFile(filepath.Join(work, "request.json"))
	if err != nil {
		return err
	}
	agent := os.Getenv("KLING_GUEST_AGENT")
	if agent == "" {
		agent = filepath.Join(libDir(), "kling-guest")
	}
	// Disco de la VM: blobs (y sus trozos), capas descomprimidas y el ext4.
	var blobsMiB int64
	for _, d := range plan.Blobs {
		if f, size, err := blobVerificado(caches, d); err == nil {
			f.Close()
			blobsMiB += size >> 20
		}
	}
	diskMiB := int(min(768+5*blobsMiB, int64(maxMB)*8+1536))

	if _, err := os.Stat(s.mgr.ImageFile(ToolsImage)); err != nil {
		return fmt.Errorf("-isolate needs the %q image: run `kling image build-tools`", ToolsImage)
	}
	tVM := time.Now()
	mc, err := s.mgr.Run(ctx, api.RunRequest{Name: "build-" + req.Name, Image: ToolsImage, VCPUs: 2, MemMiB: 2048,
		DiskMiB: diskMiB, Egress: "none", AllowExec: true})
	if err != nil {
		return fmt.Errorf("starting the tools microVM: %w", err)
	}
	defer func() { _ = s.mgr.Remove(mc.ID) }()
	base := "http://" + mc.Addr(api.GuestPort)
	if err := esperarInvitado(ctx, base, 60*time.Second); err != nil {
		return fmt.Errorf("the tools microVM did not start listening: %w", err)
	}

	fmt.Fprintf(salida, "isolate: tools microVM up in %.1f s\n", time.Since(tVM).Seconds())
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	dstExt4 := filepath.Join(out, req.Name+".ext4")
	f, err := os.OpenFile(dstExt4, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	var rec bytes.Buffer
	g := invitadoHTTP{base: base, cli: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
	log, err := aplanarEnInvitado(ctx, g, cargaVM{Name: req.Name, Request: rb, Agent: agent, Plan: plan, Caches: caches,
		MaxImage: int64(maxMB) << 23, Log: func(format string, a ...any) { fmt.Fprintf(salida, "isolate: "+format+"\n", a...) }}, f, &rec)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if log != "" {
		salida.WriteString(log)
		if !strings.HasSuffix(log, "\n") {
			salida.WriteByte('\n')
		}
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "recipe.json"), rec.Bytes(), 0o644); err != nil {
		return err
	}
	if u != nil {
		// adoptarSalida y readRecipeHints esperan lo del usuario de construcción.
		for _, p := range []string{dstExt4, filepath.Join(work, "recipe.json")} {
			if err := os.Lchown(p, int(u.UID), int(u.GID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// esperarInvitado espera a que el agente conteste /healthz.
func esperarInvitado(ctx context.Context, base string, limit time.Duration) error {
	cli := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := cli.Do(req)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return last
}
