package machine

// Carpetas compartidas: la copia (un ext4 de solo lectura construido al subir)
// y las vivas (ro/rw, servidas por internal/share mientras la máquina corre).
// Ver docs/compartir.md.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	hostshare "github.com/juan52878911/kindling/internal/share"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/share"
)

// ShareConfig es la configuración de carpetas compartidas del daemon.
type ShareConfig struct {
	// Roots son los directorios del host bajo los que se puede compartir en
	// vivo. Vacío = ninguno: ro y rw se rechazan.
	Roots []string
	// CopyMaxBytes acota el contenido de una subida en modo copy.
	CopyMaxBytes int64
}

// DefaultShareCopyMax es el tope por defecto de una copia.
const DefaultShareCopyMax = 1 << 30

// Errores con código propio en el API.
var (
	// ErrSharesCommit: una máquina con carpetas compartidas no se convierte en
	// snapshot (409).
	ErrSharesCommit = errors.New("machines with shared folders cannot be committed")
	// ErrShareRequest: la petición de carpetas no vale (400).
	ErrShareRequest = errors.New("invalid share")
)

// SetShareConfig fija de dónde lee el manager su configuración de carpetas. Se
// consulta en cada arranque: cambiar daemon.share_roots no pide reiniciar.
func (m *Manager) SetShareConfig(f func() ShareConfig) { m.shareCfg = f }

func (m *Manager) shareConfig() ShareConfig {
	var c ShareConfig
	if m.shareCfg != nil {
		c = m.shareCfg()
	}
	if c.CopyMaxBytes <= 0 {
		c.CopyMaxBytes = DefaultShareCopyMax
	}
	return c
}

// ShareRoots devuelve los directorios permitidos, para GET /info.
func (m *Manager) ShareRoots() []string { return m.shareConfig().Roots }

// ── subidas del modo copy ────────────────────────────────────────────────────

const (
	// maxPendingUploads acota las subidas que aún nadie ha usado: cada una es un
	// ext4 de hasta CopyMaxBytes en el disco del daemon.
	maxPendingUploads = 8
	// uploadTTL es cuánto vive una subida sin usar.
	uploadTTL = time.Hour
)

var reUploadID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (m *Manager) uploadsDir() string          { return filepath.Join(m.root, "shares", "uploads") }
func (m *Manager) uploadPath(id string) string { return filepath.Join(m.uploadsDir(), id+".ext4") }
func shareImagePath(dir string, i int) string {
	return filepath.Join(dir, "share"+strconv.Itoa(i)+".ext4")
}
func (m *Manager) uploadTreePath(id string) string { return filepath.Join(m.uploadsDir(), id+".tree") }

// StageShareUpload recibe el tar de una carpeta, lo valida y construye su ext4.
// Devuelve el id con el que pedirla al arrancar una máquina.
func (m *Manager) StageShareUpload(ctx context.Context, r io.Reader) (*api.ShareUpload, error) {
	cfg := m.shareConfig()
	dir := m.uploadsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m.gcUploads()
	if n := m.pendingUploads(); n >= maxPendingUploads {
		return nil, fmt.Errorf("%w: %d uploads are waiting to be used; start machines with them or wait for them to expire",
			ErrShareRequest, n)
	}

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	tree := m.uploadTreePath(id)
	if err := os.Mkdir(tree, 0o700); err != nil {
		return nil, err
	}
	// El árbol extraído no sobrevive a esta llamada, salga bien o mal: lo que
	// queda es el ext4.
	defer os.RemoveAll(tree)

	// Con margen para las cabeceras del tar: el tope que importa, el del
	// contenido, lo aplica Extract.
	body := io.LimitReader(r, cfg.CopyMaxBytes+cfg.CopyMaxBytes/8+64<<20)
	st, err := hostshare.Extract(body, tree, cfg.CopyMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrShareRequest, err)
	}

	img := m.uploadPath(id)
	if err := buildShareImage(ctx, tree, img, st); err != nil {
		_ = os.Remove(img)
		return nil, err
	}
	fi, err := os.Stat(img)
	if err != nil {
		return nil, err
	}
	return &api.ShareUpload{ID: id, Bytes: st.Bytes, Files: st.Files, Dirs: st.Dirs, Links: st.Symlinks,
		ImageBytes: fi.Size()}, nil
}

// buildShareImage construye el ext4 de una copia con el contenido de tree.
//
// Sin journal (se monta en solo lectura, y con noload) y del tamaño justo con
// holgura: lo que ocupan los datos en bloques de 4 KiB, más un 15 % y 16 MiB de
// metadatos. Si aun así no cabe, se reintenta una vez con el doble.
func buildShareImage(ctx context.Context, tree, img string, st hostshare.CopyStats) error {
	inodes := int64(st.Entries()) + 256
	mib := (st.Blocks*4096*115/100 + inodes*256 + 16<<20 + 1<<20 - 1) >> 20
	if mib < 16 {
		mib = 16
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		_ = os.Remove(img)
		if err := os.WriteFile(img, nil, 0o600); err != nil {
			return err
		}
		if err := os.Truncate(img, mib<<20); err != nil {
			return err
		}
		// El plazo mata a mke2fs si se atasca: un proceso colgado aquí dejaría
		// la subida sin respuesta y el fichero a medias.
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		out, err := e2fsCmd(cctx, "mkfs.ext4", "-q", "-F", "-b", "4096", "-I", "256", "-m", "0",
			"-N", strconv.FormatInt(inodes, 10), "-O", "^has_journal",
			"-E", "root_owner=0:0,nodiscard", "-L", "kling-share", "-d", tree, img).CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("building the share image: %v: %s", err, bytes.TrimSpace(out))
		mib *= 2
	}
	return lastErr
}

func (m *Manager) pendingUploads() int {
	ents, _ := os.ReadDir(m.uploadsDir())
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ext4") {
			n++
		}
	}
	return n
}

// gcUploads borra las subidas que nadie usó a tiempo y los árboles que dejó
// una subida interrumpida (un daemon que murió a mitad).
func (m *Manager) gcUploads() {
	ents, err := os.ReadDir(m.uploadsDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || time.Since(fi.ModTime()) < uploadTTL {
			continue
		}
		_ = os.RemoveAll(filepath.Join(m.uploadsDir(), e.Name()))
	}
}

// ── validación ───────────────────────────────────────────────────────────────

// resolvedShare es una carpeta pedida, ya validada.
type resolvedShare struct {
	att    api.ShareAttachment
	upload string // copy: el ext4 subido, que se mueve al directorio de la máquina
}

// resolveShares valida las carpetas de una petición contra los volúmenes que
// ya se van a montar.
func (m *Manager) resolveShares(req api.RunRequest, vols []resolvedVolume) ([]resolvedShare, error) {
	if len(req.Shares) == 0 {
		return nil, nil
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrShareRequest, fmt.Sprintf(format, a...))
	}
	if len(req.Shares) > share.MaxShares {
		return nil, bad("%d shared folders requested; the maximum is %d", len(req.Shares), share.MaxShares)
	}
	mounts := map[string]bool{}
	for _, v := range vols {
		mounts[v.mount] = true
	}
	var cfg *ShareConfig
	copies := 0
	out := make([]resolvedShare, 0, len(req.Shares))
	for _, s := range req.Shares {
		mode := s.Mode
		if mode == "" {
			mode = share.ModeCopy
		}
		if err := share.ValidMount(s.Mount); err != nil {
			return nil, bad("%v", err)
		}
		if mounts[s.Mount] {
			return nil, bad("%s is already used by a volume or another share", s.Mount)
		}
		for mp := range mounts {
			if strings.HasPrefix(s.Mount, mp+"/") || strings.HasPrefix(mp, s.Mount+"/") {
				return nil, bad("%s and %s are nested: one would hide the other", s.Mount, mp)
			}
		}
		mounts[s.Mount] = true

		rs := resolvedShare{att: api.ShareAttachment{Mode: mode, Mount: s.Mount}}
		switch mode {
		case share.ModeCopy:
			if !reUploadID.MatchString(s.Upload) {
				return nil, bad("mode copy needs an upload id (POST /shares/uploads); got %q", s.Upload)
			}
			p := m.uploadPath(s.Upload)
			fi, err := os.Stat(p)
			if err != nil {
				return nil, bad("upload %s not found (expired, already used, or never finished)", s.Upload)
			}
			rs.upload = p
			// Lo que el cliente dijo que subía: solo para enseñarlo.
			rs.att.Source = s.Source
			rs.att.ImageBytes = fi.Size()
			copies++
		case share.ModeRO, share.ModeRW:
			if cfg == nil {
				c := m.shareConfig()
				cfg = &c
			}
			src, err := m.liveSource(s.Source, cfg.Roots)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrShareRequest, err)
			}
			rs.att.Source = src
		default:
			return nil, bad("invalid mode %q: use copy, ro or rw", s.Mode)
		}
		out = append(out, rs)
	}
	// Cada copia es un disco más; con los volúmenes, el invitado tiene un
	// número limitado de dispositivos.
	if len(vols)+copies > maxDataDisks {
		return nil, bad("%d volumes plus %d copied folders: a machine takes at most %d disks between both",
			len(vols), copies, maxDataDisks)
	}
	return out, nil
}

// maxDataDisks es el máximo de discos de datos (volúmenes + copias) por máquina.
const maxDataDisks = 8

// liveSource valida el directorio de una carpeta viva y lo devuelve resuelto.
func (m *Manager) liveSource(src string, roots []string) (string, error) {
	if len(roots) == 0 {
		return "", errors.New("live shares (ro, rw) are disabled on this daemon.\n" +
			"Allow a host directory with `kling config set daemon.share_roots /path/to/dir` " +
			"(as the user the daemon runs as: sudo on Linux) or KLING_SHARE_ROOTS, or use mode copy")
	}
	if src == "" || !filepath.IsAbs(src) {
		return "", fmt.Errorf("a live share needs an absolute path on the daemon host, got %q", src)
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", fmt.Errorf("share source %s: %w", src, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("share source %s is not a directory", src)
	}
	// Nada que contenga la raíz de datos de kindling: ahí están los volúmenes,
	// los snapshots y las máquinas de todos.
	if dataRoot, err := filepath.EvalSymlinks(m.root); err == nil && within(dataRoot, real) {
		return "", fmt.Errorf("%s contains kindling's data root (%s) and cannot be shared", src, m.root)
	}
	var allowed []string
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		allowed = append(allowed, rr)
		if within(real, rr) {
			return real, nil
		}
	}
	return "", fmt.Errorf("%s is not under any allowed share root (daemon.share_roots: %s)",
		src, strings.Join(roots, ", "))
}

// within dice si p es dir o está dentro de dir (rutas ya resueltas).
func within(p, dir string) bool {
	if p == dir || dir == "/" {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// placeCopies mueve los ext4 subidos al directorio de la máquina y devuelve los
// discos que hay que enganchar detrás de los volúmenes. La subida se consume:
// una segunda máquina con el mismo id no la encuentra.
func (m *Manager) placeCopies(dir string, shares []resolvedShare) ([]resolvedVolume, error) {
	var out []resolvedVolume
	for i, s := range shares {
		if s.upload == "" {
			continue
		}
		dst := shareImagePath(dir, i)
		if err := os.Rename(s.upload, dst); err != nil {
			return nil, fmt.Errorf("taking upload for %s: %w", s.att.Mount, err)
		}
		if err := m.priv.Own(dst); err != nil {
			return nil, err
		}
		out = append(out, resolvedVolume{path: dst, mount: s.att.Mount, readOnly: true})
	}
	return out, nil
}

// copyImages son los ext4 de las copias de una máquina, en orden de disco.
func (m *Manager) copyImages(mc *api.Machine) []string {
	var out []string
	for i, s := range mc.Shares {
		if !s.Live() {
			out = append(out, shareImagePath(m.dir(mc.ID), i))
		}
	}
	return out
}

func hasLiveShares(mc *api.Machine) bool {
	for _, s := range mc.Shares {
		if s.Live() {
			return true
		}
	}
	return false
}

// ── carpetas vivas: el supervisor ────────────────────────────────────────────

// shareSup lleva las conexiones de las carpetas vivas de cada máquina.
//
// Una goroutine por carpeta: hace attach, sirve la sesión mientras dure y, si
// se corta con la máquina corriendo, vuelve a intentarlo. Congelar, parar y
// borrar paran las goroutines ANTES de tocar el VMM (stop), y congelar además
// impide que el vigilante las vuelva a lanzar mientras dura (hold).
type shareSup struct {
	mu   sync.Mutex
	runs map[string]*shareRun
	held map[string]bool
}

type shareRun struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	tags   []*shareTag
	// done se cierra cuando todas sus goroutines han terminado (la máquina dejó
	// de correr, o un error permanente). Una ejecución terminada se puede
	// sustituir por otra.
	done chan struct{}
}

// permanent dice si alguna de sus carpetas acabó en un error que no se arregla
// reintentando: el vigilante no la relanza sola.
func (r *shareRun) permanent() bool {
	for _, t := range r.tags {
		select {
		case <-t.first:
			if t.permErr != nil {
				return true
			}
		default:
		}
	}
	return false
}

// shareTag es el estado de UNA carpeta viva.
type shareTag struct {
	tag int
	mu  sync.Mutex
	// status: "attached", "detached" o "error: ...".
	status string
	// first se cierra en el primer attach o en el primer error permanente.
	first     chan struct{}
	firstOnce sync.Once
	permErr   error
}

func (t *shareTag) set(status string) {
	t.mu.Lock()
	t.status = status
	t.mu.Unlock()
}

func (t *shareTag) get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *shareTag) settle(err error) {
	t.firstOnce.Do(func() {
		t.permErr = err
		close(t.first)
	})
}

func newShareSup() *shareSup {
	return &shareSup{runs: map[string]*shareRun{}, held: map[string]bool{}}
}

// sup devuelve el supervisor, creándolo la primera vez: así también lo tiene un
// Manager construido a mano (los tests).
func (m *Manager) sup() *shareSup {
	m.sharesOnce.Do(func() {
		if m.shares == nil {
			m.shares = newShareSup()
		}
	})
	return m.shares
}

// startShares lanza las conexiones de las carpetas vivas de id si no corren ya.
// No hace nada si la máquina no corre, no tiene carpetas vivas o está retenida.
func (m *Manager) startShares(id string) { m.startSharesIf(id, true) }

func (m *Manager) startSharesIf(id string, afterPermanent bool) {
	mc, ok := m.get(id)
	if !ok || mc.State != api.StateRunning || !hasLiveShares(mc) {
		return
	}
	sup := m.sup()
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.held[id] {
		return
	}
	if r := sup.runs[id]; r != nil {
		select {
		case <-r.done:
			if r.permanent() && !afterPermanent {
				return
			}
		default:
			return // sigue corriendo
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &shareRun{cancel: cancel, done: make(chan struct{})}
	for tag, s := range mc.Shares {
		if !s.Live() {
			continue
		}
		t := &shareTag{tag: tag, status: "detached", first: make(chan struct{})}
		run.tags = append(run.tags, t)
		run.wg.Add(1)
		go func(s api.ShareAttachment) {
			defer run.wg.Done()
			m.shareLoop(ctx, id, s, t)
		}(s)
	}
	go func() { run.wg.Wait(); close(run.done) }()
	sup.runs[id] = run
}

// stopShares corta las conexiones de id y espera a que terminen. Lo que el
// invitado tuviera en vuelo recibe EIO.
func (m *Manager) stopShares(id string) {
	sup := m.sup()
	sup.mu.Lock()
	run := sup.runs[id]
	delete(sup.runs, id)
	sup.mu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	run.wg.Wait()
}

// holdShares impide que se vuelvan a lanzar (congelar). releaseShares lo
// levanta y, si la máquina sigue corriendo (congelar falló), las relanza.
func (m *Manager) holdShares(id string) {
	m.sup().mu.Lock()
	m.sup().held[id] = true
	m.sup().mu.Unlock()
}

func (m *Manager) releaseShares(id string) {
	m.sup().mu.Lock()
	delete(m.sup().held, id)
	m.sup().mu.Unlock()
	m.startShares(id)
}

// ensureShares lanza las que falten. Lo llama el vigilante: tras reiniciar el
// daemon, las máquinas readoptadas recuperan sus carpetas por aquí.
func (m *Manager) ensureShares() {
	m.mu.RLock()
	var ids []string
	for id, mc := range m.byID {
		if mc.State == api.StateRunning && hasLiveShares(mc) {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.startSharesIf(id, false)
	}
}

// waitShares espera al primer attach de todas las carpetas vivas de id. Un
// error permanente (agente viejo, kernel sin FUSE) se devuelve en el acto.
func (m *Manager) waitShares(ctx context.Context, id string, timeout time.Duration) error {
	m.sup().mu.Lock()
	run := m.sup().runs[id]
	m.sup().mu.Unlock()
	if run == nil {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, t := range run.tags {
		select {
		case <-t.first:
			if t.permErr != nil {
				return t.permErr
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("shared folder %d was not attached within %s: %s", t.tag, timeout, t.get())
		}
	}
	return nil
}

// shareStatus es el estado de las carpetas vivas de id, por tag.
func (m *Manager) shareStatus(id string) map[int]string {
	m.sup().mu.Lock()
	run := m.sup().runs[id]
	m.sup().mu.Unlock()
	out := map[int]string{}
	if run == nil {
		return out
	}
	for _, t := range run.tags {
		out[t.tag] = t.get()
	}
	return out
}

// decorarShares copia las carpetas de out y rellena el estado de las vivas.
// Copia porque out comparte el slice con la máquina guardada.
func (m *Manager) decorarShares(out *api.Machine) {
	if len(out.Shares) == 0 {
		return
	}
	st := map[int]string{}
	if out.State == api.StateRunning && hasLiveShares(out) {
		st = m.shareStatus(out.ID)
	}
	sh := make([]api.ShareAttachment, len(out.Shares))
	copy(sh, out.Shares)
	for i := range sh {
		if sh[i].Live() {
			sh[i].Status = st[i]
			if sh[i].Status == "" {
				sh[i].Status = "detached"
			}
		}
	}
	out.Shares = sh
}

// errPermanente marca un fallo de attach que no se arregla reintentando.
type errPermanente struct{ error }

func (e errPermanente) Unwrap() error { return e.error }

// shareLoop mantiene conectada una carpeta viva mientras la máquina corra.
func (m *Manager) shareLoop(ctx context.Context, id string, s api.ShareAttachment, t *shareTag) {
	backoff := 200 * time.Millisecond
	for ctx.Err() == nil {
		mc, ok := m.get(id)
		if !ok || mc.State != api.StateRunning {
			t.set("detached")
			return
		}
		err := m.attachOnce(ctx, mc, s, t)
		if ctx.Err() != nil {
			t.set("detached")
			return
		}
		var perm errPermanente
		if errors.As(err, &perm) {
			t.set("error: " + err.Error())
			t.settle(err)
			log.Printf("%s: shared folder %s: %v", mc.Name, s.Mount, err)
			return
		}
		if err != nil {
			t.set("error: " + err.Error())
		} else {
			t.set("detached")
			backoff = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// shareClient hace los attach. Sin reutilizar conexiones: cada una la secuestra
// su sesión.
var shareClient = &http.Client{Transport: &http.Transport{
	DisableKeepAlives:     true,
	ResponseHeaderTimeout: 30 * time.Second,
}}

// attachOnce conecta la carpeta s con el agente de mc y la sirve hasta que la
// sesión se corta. nil = se sirvió y se cortó (hay que reconectar).
func (m *Manager) attachOnce(ctx context.Context, mc *api.Machine, s api.ShareAttachment, t *shareTag) error {
	if !mc.Reachable() {
		return errors.New("the machine is not reachable yet")
	}
	srv, err := hostshare.Open(s.Source, s.Mode == share.ModeRO)
	if err != nil {
		return fmt.Errorf("opening %s: %w", s.Source, err)
	}
	defer srv.Close()

	body, _ := json.Marshal(share.Attach{Tag: t.tag, Mount: s.Mount, Mode: s.Mode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+mc.Addr(api.GuestPort)+share.AttachPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", share.Proto)
	resp, err := shareClient.Do(req)
	if err != nil {
		return fmt.Errorf("talking to the guest agent: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		text := strings.TrimSpace(string(msg))
		// Sin la cabecera, quien contesta no es un agente que sepa de
		// carpetas: kling-guest anterior (404) o el puente MCP (un 400 suyo).
		// Un 5xx sin ella puede ser el reenvío de macOS antes de que el agente
		// escuche, y se reintenta.
		if resp.Header.Get(share.HeaderAgent) == "" && resp.StatusCode < 500 {
			return errPermanente{fmt.Errorf("the guest agent in image %q is too old for live shares (kindling v0.10); "+
				"rebuild the image (kling images toolchain, or kling images build -builder base)", mc.Image)}
		}
		switch resp.StatusCode {
		case http.StatusNotImplemented:
			return errPermanente{fmt.Errorf("the guest cannot mount live shares: %s", text)}
		case http.StatusBadRequest, http.StatusConflict, http.StatusForbidden:
			return errPermanente{fmt.Errorf("the guest agent refused the share: %s", text)}
		case http.StatusBadGateway, http.StatusServiceUnavailable:
			// El agente aún no escucha (macOS: el reenvío acepta igual).
			return fmt.Errorf("the guest agent is not ready: %s", resp.Status)
		}
		return fmt.Errorf("guest agent: %s: %s", resp.Status, text)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return errors.New("the guest connection could not be taken over")
	}
	t.set("attached")
	t.settle(nil)
	err = srv.Serve(ctx, rwc)
	_ = rwc.Close()
	if err != nil && ctx.Err() == nil {
		log.Printf("%s: shared folder %s: session ended: %v", mc.Name, s.Mount, err)
	}
	return nil
}
