package aigw

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// MEJORA CONTINUA: que Chispa aprenda lo que otros resolvieron por él.
//
// Cuando Chispa duda, alguien más lento contesta: VON en la cascada, el
// codificador en una tarea de domótica, el propio cliente (escalate: true y
// «quien llama decide») o una persona. Este fichero guarda esos casos, opt-in
// por tarea, para que `kling ai retrain` entrene un Chispa nuevo con ellos y
// solo lo promocione si gana en un conjunto de CONFIANZA (docs/mejora-continua.md).
//
// Lo que enseñó el intento anterior (`kling ai calibrate` con VON de maestro, que
// empeoró a Chispa: cobertura 23,5 → 12,3 %, precisión 0,851 → 0,783) manda el
// diseño:
//
//  1. El maestro era PEOR que el alumno donde importaba (VON 42 % frente a
//     Chispa 57,5 % en lo escalado). Aquí la etiqueta de un maestro solo entra
//     si ese maestro está validado contra etiquetas de verdad (revisiones
//     humanas o un registro de `kling ai eval` que lo respalde), y además cada
//     muestra pasa un filtro de acuerdo (learn_review.go). Por defecto, la
//     única fuente que se trata como verdad es una persona.
//  2. Se medía CONCORDANCIA con el maestro, no acierto. Aquí toda puerta se
//     mide contra el conjunto apartado de confianza (learn.heldout), nunca
//     contra un maestro.
//  3. La muestra estaba sesgada hacia lo dudoso. Aquí cada reentreno lleva
//     entero el conjunto de oro (learn.gold), que nunca se descarta, y un tope
//     por clase para lo nuevo.
//  4. Solo se movían umbrales, sin texto. Aquí se guarda el texto (opt-in,
//     acotado, con un filtro de secretos) o solo su hash.

// LearnConfig es el bloque "learn" de una tarea de clasificación o de domótica.
type LearnConfig struct {
	// Capture guarda las escaladas: "text" (el texto, pasado por un filtro de
	// secretos), "hash" (solo su sha256: nada legible en disco, y entonces
	// solo entrena lo que un humano mande con su propio texto) u "off"/"" (no
	// se captura; el bucle vive solo de lo que se mande a /v1/feedback).
	Capture string `json:"capture,omitempty"`
	MaxMB   int    `json:"max_mb,omitempty"`   // capturas en disco por tarea; 64
	MaxDays int    `json:"max_days,omitempty"` // antigüedad de una captura; 30
	// Gold es el ancla: el JSONL con el que se entrenó el modelo. Entra ENTERO
	// en cada reentreno y nunca se descarta. Relativo al registro.
	Gold string `json:"gold,omitempty"`
	// Heldout es el conjunto apartado de confianza: etiquetas de verdad que
	// ningún entrenamiento ve. Decide la promoción. Relativo al registro.
	Heldout string `json:"heldout,omitempty"`
	// Valid es la validación para calibrar (temperatura y umbrales). Vacío = un
	// 10 % del oro. Las etiquetas humanas aportan además un 20 % suyo.
	Valid string `json:"valid,omitempty"`
	// MinVotes son los votos de maestros validados, todos de acuerdo, que
	// necesita una muestra para entrar sin revisión; 2.
	MinVotes int `json:"min_votes,omitempty"`
	// TopK: la etiqueta del maestro tiene que estar entre las K más probables
	// de Chispa; 3. Un maestro que propone algo que Chispa ni considera suele
	// ser el que se equivoca (o un caso que merece ojos humanos).
	TopK int `json:"top_k,omitempty"`
	// TeacherWeight es el peso de una muestra aceptada de un maestro frente a
	// una de oro o humana (1); 0,5.
	TeacherWeight float64 `json:"teacher_weight,omitempty"`
	// MaxPerClass acota las muestras de maestros por clase en un reentreno
	// (0 = máx(50, las que la clase tiene en el oro)): lo nuevo no puede
	// ahogar a las clases raras ni mover el reparto lejos del ancla.
	MaxPerClass int `json:"max_per_class,omitempty"`
	// MinChecks son los casos revisados por una persona con voto de un
	// maestro que hacen falta para validarlo; 30. AcceptPrecision es la
	// precisión (estimada con aciertos/(n+1)) que debe tener, en esos casos,
	// lo que el filtro de acuerdo habría aceptado; 0,9.
	MinChecks       int     `json:"min_checks,omitempty"`
	AcceptPrecision float64 `json:"accept_precision,omitempty"`
	// TrustTeachers fuerza maestros sin validar (el -force de la decisión:
	// queda escrito en el registro y en cada informe).
	TrustTeachers []string `json:"trust_teachers,omitempty"`
	// VONVotes son respuestas extra de VON (T=0,4, semillas nuevas) por
	// escalada, en segundo plano: la autoconsistencia que cuenta como votos.
	// 0..2; solo con la cascada activa.
	VONVotes int `json:"von_votes,omitempty"`
	// EscalationMS es lo que cuesta una escalada, para estimar el tiempo
	// ahorrado (0 = la latencia media medida de VON en la tarea, si la hay).
	EscalationMS float64 `json:"escalation_ms,omitempty"`
	// WindowMinutes es la ventana de la tasa de escalado de /metrics; 60.
	WindowMinutes int `json:"window_minutes,omitempty"`
}

// Modos de captura.
const (
	CaptureText = "text"
	CaptureHash = "hash"
	CaptureOff  = "off"
)

// Límites del almacén. Las etiquetas humanas no caducan: son el bien escaso.
const (
	maxLearnMB       = 4096
	maxFeedbackBytes = 64 << 20
	maxFeedbackItems = 1000
	maxLearnQueue    = 4096
	maxLearnFieldLen = 256
	maxByLen         = 64
	learnTopN        = 5
)

// withDefaults devuelve la configuración con los valores por defecto puestos.
func (l LearnConfig) withDefaults() LearnConfig {
	if l.MaxMB == 0 {
		l.MaxMB = 64
	}
	if l.MaxDays == 0 {
		l.MaxDays = 30
	}
	if l.MinVotes == 0 {
		l.MinVotes = 2
	}
	if l.TopK == 0 {
		l.TopK = 3
	}
	if l.TeacherWeight == 0 {
		l.TeacherWeight = 0.5
	}
	if l.MinChecks == 0 {
		l.MinChecks = 30
	}
	if l.AcceptPrecision == 0 {
		l.AcceptPrecision = 0.9
	}
	if l.WindowMinutes == 0 {
		l.WindowMinutes = 60
	}
	return l
}

func (l LearnConfig) capturing() bool {
	return l.Capture == CaptureText || l.Capture == CaptureHash
}

var teacherRE = regexp.MustCompile(`^(ext:)?[a-z0-9][a-z0-9._-]{0,63}$`)

// validateLearn comprueba el bloque learn de una tarea (de clasificación o de
// domótica; quien llama ya rechazó las de generación).
func validateLearn(task string, l *LearnConfig, escalateTo string) []error {
	var errs []error
	bad := func(f string, a ...any) {
		errs = append(errs, fmt.Errorf("task %q: learn: %s", task, fmt.Sprintf(f, a...)))
	}
	switch l.Capture {
	case "", CaptureOff, CaptureText, CaptureHash:
	default:
		bad("capture must be text, hash or off, not %q", l.Capture)
	}
	if l.MaxMB < 0 || l.MaxMB > maxLearnMB {
		bad("max_mb must be 0..%d", maxLearnMB)
	}
	if l.MaxDays < 0 || l.MaxDays > 3650 {
		bad("max_days must be 0..3650")
	}
	if l.MinVotes < 0 || l.MinVotes > 8 {
		bad("min_votes must be 0..8")
	}
	if l.TopK < 0 || l.TopK > maxLabels {
		bad("top_k must be 0..%d", maxLabels)
	}
	if !(l.TeacherWeight >= 0 && l.TeacherWeight <= 1) {
		bad("teacher_weight must be in [0,1]")
	}
	if l.MaxPerClass < 0 {
		bad("max_per_class must be >= 0")
	}
	if l.MinChecks < 0 || l.MinChecks > 100_000 {
		bad("min_checks must be 0..100000")
	}
	if !(l.AcceptPrecision >= 0 && l.AcceptPrecision < 1) {
		bad("accept_precision must be in [0,1)")
	}
	for _, t := range l.TrustTeachers {
		if !teacherRE.MatchString(t) {
			bad("trust_teachers: invalid teacher name %q", t)
		}
	}
	if l.VONVotes < 0 || l.VONVotes > 2 {
		bad("von_votes must be 0..2")
	}
	if l.VONVotes > 0 && escalateTo == "" {
		bad("von_votes asks the escalate_to model; set it or drop von_votes")
	}
	if !(l.EscalationMS >= 0 && l.EscalationMS < 1e7) {
		bad("escalation_ms must be >= 0")
	}
	if l.WindowMinutes < 0 || l.WindowMinutes > 24*60 {
		bad("window_minutes must be 0..1440")
	}
	return errs
}

// ---- lo que se guarda

// CaptureRecord es una escalada (o una auditoría) capturada: una línea de
// ai-data/<tarea>/captures/<AAAA-MM-DD>.jsonl. Es el formato estable del
// bucle; docs/mejora-continua.md lo documenta para quien quiera escribirlo o
// leerlo desde fuera.
type CaptureRecord struct {
	ID   string    `json:"id"`
	Task string    `json:"task"`
	TS   time.Time `json:"ts"`
	// Kind: "escalated" (Chispa dudó) o "audit" (Chispa contestó confiado y
	// una auditoría le preguntó también a VON).
	Kind string `json:"kind"`
	// Text solo con capture "text" (ya filtrado); TextSHA256 siempre, del
	// texto original: enlaza la captura con lo que mande un humano después.
	Text       string         `json:"text,omitempty"`
	TextSHA256 string         `json:"text_sha256"`
	Fields     map[string]any `json:"fields,omitempty"`
	Chispa     CaptureChispa  `json:"chispa"`
	Teachers   []TeacherVote  `json:"teachers,omitempty"`
}

// CaptureChispa es lo que dijo Chispa, y qué versión lo dijo.
type CaptureChispa struct {
	Version string             `json:"version,omitempty"`
	SHA256  string             `json:"sha256,omitempty"`
	Label   string             `json:"label"`
	Prob    float64            `json:"prob"`
	Top     []chispa.ClassProb `json:"top,omitempty"`
}

// TeacherVote es la respuesta de una capa más lenta. Name es el modelo del
// registro (un VON o un codificador) o "ext:<nombre>" si lo mandó un cliente
// por /v1/feedback. Conf 0 = sin confianza conocida (VON con gramática no la da).
type TeacherVote struct {
	Name  string  `json:"name"`
	Label string  `json:"label"`
	Conf  float64 `json:"conf,omitempty"`
}

// FeedbackRecord es una línea de ai-data/<tarea>/feedback.jsonl: lo que dijo
// una persona (Source "human": la verdad) o un maestro externo (Source
// "teacher": un voto más, que tiene que validarse como cualquier maestro).
type FeedbackRecord struct {
	ID      string         `json:"id,omitempty"`
	Task    string         `json:"task"`
	TS      time.Time      `json:"ts"`
	Source  string         `json:"source"`           // human | teacher
	Action  string         `json:"action,omitempty"` // human: confirm | correct | discard
	Label   string         `json:"label,omitempty"`
	Teacher string         `json:"teacher,omitempty"`
	Conf    float64        `json:"conf,omitempty"`
	By      string         `json:"by,omitempty"`
	Text    string         `json:"text,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// ---- el almacén y su escritor

// learner es el almacén de la mejora continua de un gateway: un directorio por
// tarea bajo DataDir y UN escritor en segundo plano. La petición solo encola
// (sin bloquear: si la cola está llena, la captura se pierde y se cuenta);
// filtrar, serializar y escribir a disco ocurre fuera del camino de µs.
type learner struct {
	dir string // ai-data; "" = sin almacén (sin registro en disco)

	q    chan learnJob
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once

	idPrefix string
	idSeq    atomic.Uint64

	fbMu sync.Mutex // escrituras de feedback.jsonl (rutas de administración)

	mu        sync.Mutex
	counts    map[string]uint64          // tarea|resultado -> capturas (written, queue_full, cap, dedup, error)
	manifests map[string]*versionsFile   // tarea -> versiones (learn_retrain.go)
	pending   map[string]int             // tarea -> casos pendientes de revisión (último cálculo)
	lastRun   map[string]*RetrainSummary // tarea -> último reentreno
}

type learnJob struct {
	rec   *CaptureRecord
	text  string // el original: el hash sale de aquí antes de filtrar
	mode  string
	maxB  int64
	days  int
	flush chan struct{}
}

func newLearner(dir string) *learner {
	var b [6]byte
	_, _ = rand.Read(b[:])
	l := &learner{
		dir: dir, q: make(chan learnJob, maxLearnQueue), stop: make(chan struct{}),
		idPrefix: hex.EncodeToString(b[:]),
		counts:   map[string]uint64{}, manifests: map[string]*versionsFile{},
		pending: map[string]int{}, lastRun: map[string]*RetrainSummary{},
	}
	if dir != "" {
		l.wg.Add(1)
		go l.run()
	}
	return l
}

// newID es el id de una respuesta: prefijo aleatorio del proceso y un
// contador. Barato (el camino confiado es de µs) y único por gateway.
func (l *learner) newID() string {
	return l.idPrefix + "-" + strconv.FormatUint(l.idSeq.Add(1), 36)
}

func (l *learner) count(task, outcome string) {
	l.mu.Lock()
	l.counts[task+"|"+outcome]++
	l.mu.Unlock()
}

// taskDir es el directorio de una tarea. Los nombres de tarea pasan nameRE:
// no pueden salir de dir.
func (l *learner) taskDir(task string) string { return filepath.Join(l.dir, task) }

// enqueue encola una captura sin bloquear nunca.
func (l *learner) enqueue(lc LearnConfig, rec *CaptureRecord, text string) {
	if l.dir == "" {
		return
	}
	j := learnJob{rec: rec, text: text, mode: lc.Capture, maxB: int64(lc.MaxMB) << 20, days: lc.MaxDays}
	select {
	case l.q <- j:
	default:
		l.count(rec.Task, "queue_full")
	}
}

// flush espera a que todo lo encolado antes esté en disco.
func (l *learner) flush() {
	if l.dir == "" {
		return
	}
	done := make(chan struct{})
	select {
	case l.q <- learnJob{flush: done}:
	case <-l.stop:
		return
	}
	select {
	case <-done:
	case <-l.stop:
	}
}

func (l *learner) close() {
	l.once.Do(func() { close(l.stop) })
	l.wg.Wait()
}

// taskWriter es el fichero del día de una tarea, abierto por el escritor.
type taskWriter struct {
	day   string
	f     *os.File
	bytes int64 // de todas las capturas de la tarea
	seen  map[string]bool
}

func (l *learner) run() {
	defer l.wg.Done()
	ws := map[string]*taskWriter{}
	defer func() {
		for _, w := range ws {
			if w.f != nil {
				w.f.Close()
			}
		}
	}()
	for {
		select {
		case <-l.stop:
			return
		case j := <-l.q:
			if j.flush != nil {
				close(j.flush)
				continue
			}
			l.write(ws, j)
		}
	}
}

func (l *learner) write(ws map[string]*taskWriter, j learnJob) {
	rec := j.rec
	sum := sha256.Sum256([]byte(j.text))
	rec.TextSHA256 = hex.EncodeToString(sum[:])
	if j.mode == CaptureText {
		rec.Text = redact(j.text)
	}
	rec.Fields = redactFields(rec.Fields)
	day := rec.TS.UTC().Format("2006-01-02")
	w := ws[rec.Task]
	if w == nil || w.day != day {
		if w != nil && w.f != nil {
			w.f.Close()
		}
		delete(ws, rec.Task)
		dir := filepath.Join(l.taskDir(rec.Task), "captures")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Printf("learn %s: %v", rec.Task, err)
			l.count(rec.Task, "error")
			return
		}
		total := pruneCaptures(dir, j.maxB, j.days, day)
		f, err := os.OpenFile(filepath.Join(dir, day+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			log.Printf("learn %s: %v", rec.Task, err)
			l.count(rec.Task, "error")
			return
		}
		w = &taskWriter{day: day, f: f, bytes: total, seen: map[string]bool{}}
		ws[rec.Task] = w
	}
	// Dedup por texto dentro del día: el mismo texto da la misma predicción y,
	// casi siempre, la misma respuesta del maestro. Un duplicado no enseña nada
	// y pesaría doble.
	if w.seen[rec.TextSHA256] {
		l.count(rec.Task, "dedup")
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		l.count(rec.Task, "error")
		return
	}
	b = append(b, '\n')
	if w.bytes+int64(len(b)) > j.maxB {
		// Primero se hace sitio con lo más viejo; si ni así cabe (el día de hoy
		// ya llena el tope), se pierde y se cuenta: el disco no crece sin fin.
		w.bytes = pruneCaptures(filepath.Dir(w.f.Name()), j.maxB-int64(len(b)), j.days, day)
		if w.bytes+int64(len(b)) > j.maxB {
			l.count(rec.Task, "cap")
			return
		}
	}
	if _, err := w.f.Write(b); err != nil {
		l.count(rec.Task, "error")
		return
	}
	w.bytes += int64(len(b))
	if len(w.seen) < 200_000 {
		w.seen[rec.TextSHA256] = true
	}
	l.count(rec.Task, "written")
}

var dayFileRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

// pruneCaptures borra los ficheros de captura más viejos que maxDays y, si
// hace falta, los más antiguos hasta que el total quepa en maxB (nunca el del
// día de hoy). Devuelve el tamaño que queda.
func pruneCaptures(dir string, maxB int64, maxDays int, today string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type fi struct {
		name string
		size int64
	}
	var files []fi
	var total int64
	cutoff := ""
	if t, err := time.Parse("2006-01-02", today); err == nil && maxDays > 0 {
		cutoff = t.AddDate(0, 0, -maxDays).Format("2006-01-02")
	}
	for _, e := range ents {
		if !dayFileRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if cutoff != "" && strings.TrimSuffix(e.Name(), ".jsonl") < cutoff {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		files = append(files, fi{e.Name(), info.Size()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, f := range files {
		if total <= maxB || f.name == today+".jsonl" {
			break
		}
		if os.Remove(filepath.Join(dir, f.name)) == nil {
			total -= f.size
		}
	}
	return total
}

// ---- filtro de secretos

// Lo que parece un secreto se cambia por [redacted] antes de tocar el disco.
// Es un filtro básico y conservador (claves con nombre, tokens con prefijo
// conocido, JWT, claves privadas, tiras largas que parecen credenciales,
// correos), no una garantía: con datos sensibles, capture "hash". Corre en el
// escritor, nunca en el camino de la petición.
var (
	secretKV    = regexp.MustCompile(`(?i)\b(authorization|bearer|token|api[_-]?key|secret|password|passwd|pwd)(\s*[:=]\s*|\s+)("[^"]*"|'[^']*'|[^\s,;]+)`)
	secretLong  = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
	secretFixed = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`),
		regexp.MustCompile(`\b(sk|pk|rk)-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
	}
)

func redact(s string) string {
	for _, re := range secretFixed {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	s = secretKV.ReplaceAllString(s, "$1$2[redacted]")
	// Una tira de 32+ caracteres sin espacios que mezcla letras y dígitos es
	// casi siempre una credencial o un hash; una palabra de verdad no.
	return secretLong.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && strings.IndexFunc(m, func(r rune) bool {
			return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		}) >= 0 {
			return "[redacted]"
		}
		return m
	})
}

// redactFields filtra los campos de texto y recorta lo largo: un campo es una
// característica, no un documento.
func redactFields(f map[string]any) map[string]any {
	if len(f) == 0 {
		return nil
	}
	out := make(map[string]any, len(f))
	for k, v := range f {
		if s, ok := v.(string); ok {
			v = truncUTF8(redact(s), maxLearnFieldLen)
		}
		out[k] = v
	}
	return out
}

// ---- lectura

// readJSONL lee un JSONL acotado (líneas de 1 MiB, maxB bytes) y llama a fn
// por cada línea; las rotas se cuentan y se saltan: un fichero a medias (el
// gateway murió escribiendo) no puede tumbar un reentreno.
func readJSONL(path string, maxB int64, fn func([]byte) error) (bad int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxB))
	sc.Buffer(make([]byte, 64<<10), chispa.MaxLineBytes)
	for sc.Scan() {
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		if fn(b) != nil {
			bad++
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return bad, err
	}
	return bad, nil
}

// readCaptures lee las capturas de una tarea, de la más vieja a la más nueva.
func (l *learner) readCaptures(task string, maxB int64) ([]CaptureRecord, error) {
	dir := filepath.Join(l.taskDir(task), "captures")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if dayFileRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []CaptureRecord
	for _, n := range names {
		_, err := readJSONL(filepath.Join(dir, n), maxB, func(b []byte) error {
			var r CaptureRecord
			if err := json.Unmarshal(b, &r); err != nil || r.ID == "" {
				return errors.New("bad record")
			}
			out = append(out, r)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l *learner) feedbackPath(task string) string {
	return filepath.Join(l.taskDir(task), "feedback.jsonl")
}

func (l *learner) readFeedback(task string) ([]FeedbackRecord, error) {
	var out []FeedbackRecord
	_, err := readJSONL(l.feedbackPath(task), maxFeedbackBytes+chispa.MaxLineBytes, func(b []byte) error {
		var r FeedbackRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// ---- /v1/feedback

// FeedbackItem es una etiqueta para un caso: la de una persona (sin Teacher:
// confirmar o corregir, o descartar el caso) o la respuesta de un maestro
// externo (con Teacher). ID es el "id" de una respuesta del gateway; sin él (o
// si el caso no se capturó) hace falta Text.
type FeedbackItem struct {
	ID      string         `json:"id,omitempty"`
	Action  string         `json:"action,omitempty"` // confirm | correct | discard ("" = correct)
	Label   string         `json:"label,omitempty"`
	Teacher string         `json:"teacher,omitempty"`
	Conf    float64        `json:"conf,omitempty"`
	By      string         `json:"by,omitempty"`
	Text    string         `json:"text,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// FeedbackRequest es el cuerpo de POST /v1/feedback: un caso (los campos de
// FeedbackItem al nivel de arriba) o varios (items, hasta 1000).
type FeedbackRequest struct {
	Task string `json:"task"`
	FeedbackItem
	Items []FeedbackItem `json:"items,omitempty"`
}

// FeedbackResponse dice cuántos se guardaron.
type FeedbackResponse struct {
	Task     string `json:"task"`
	Recorded int    `json:"recorded"`
}

var idRE = regexp.MustCompile(`^[0-9a-f]{12}-[0-9a-z]{1,13}$`)

// Feedback guarda etiquetas de personas o de maestros externos. human dice si
// quien llama puede hablar como persona (el token principal): un token de
// tenant solo aporta votos de maestro, que nunca son verdad por sí solos.
func (g *Gateway) Feedback(req FeedbackRequest, human bool, tenant string) (*FeedbackResponse, error) {
	cfg := g.config()
	tc := cfg.Tasks[req.Task]
	if tc == nil {
		return nil, statusf(http.StatusNotFound, "unknown task %q", req.Task)
	}
	if tc.Learn == nil {
		return nil, statusf(http.StatusBadRequest, "task %q has no \"learn\" block: add one to record feedback", req.Task)
	}
	if g.learn.dir == "" {
		return nil, statusf(http.StatusServiceUnavailable, "this gateway has no data directory (it runs without a registry file)")
	}
	items := req.Items
	top := req.FeedbackItem
	if top.Label != "" || top.ID != "" || top.Text != "" || top.Action != "" || top.Teacher != "" {
		if len(items) > 0 {
			return nil, statusf(http.StatusBadRequest, "send one item at the top level or a list in \"items\", not both")
		}
		items = []FeedbackItem{top}
	}
	if len(items) == 0 || len(items) > maxFeedbackItems {
		return nil, statusf(http.StatusBadRequest, "need 1..%d items", maxFeedbackItems)
	}
	now := time.Now().UTC()
	lines := make([][]byte, 0, len(items))
	for i, it := range items {
		if err := checkFeedback(it); err != nil {
			return nil, statusf(http.StatusBadRequest, "item %d: %v", i+1, err)
		}
		rec := FeedbackRecord{ID: it.ID, Task: req.Task, TS: now, Label: it.Label, By: it.By, Fields: redactFields(it.Fields)}
		if it.Text != "" {
			rec.Text = redact(it.Text)
		}
		if it.Teacher != "" {
			// Un maestro externo siempre lleva "ext:" (y el tenant, si no es el
			// principal): no puede hacerse pasar por un modelo del registro, cuya
			// validación puede venir de un registro de evaluación.
			name := strings.TrimPrefix(it.Teacher, "ext:")
			if tenant != "" && tenant != "default" {
				name = tenant + "." + name
			}
			rec.Source, rec.Teacher, rec.Conf = "teacher", "ext:"+name, it.Conf
		} else {
			if !human {
				return nil, statusf(http.StatusForbidden, "human labels need the gateway's main token (a tenant token can only report a teacher's answer, with \"teacher\")")
			}
			rec.Source, rec.Action = "human", it.Action
			if rec.Action == "" {
				rec.Action = "correct"
			}
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		lines = append(lines, append(b, '\n'))
	}
	if err := g.learn.appendFeedback(req.Task, lines); err != nil {
		return nil, err
	}
	return &FeedbackResponse{Task: req.Task, Recorded: len(lines)}, nil
}

func checkFeedback(it FeedbackItem) error {
	if it.ID != "" && !idRE.MatchString(it.ID) {
		return fmt.Errorf("invalid id %q", it.ID)
	}
	if it.ID == "" && it.Text == "" {
		return errors.New("needs the id of a gateway answer or the text")
	}
	if len(it.Text) > maxText {
		return fmt.Errorf("text larger than %d bytes", maxText)
	}
	if len(it.Fields) > 64 {
		return errors.New("at most 64 fields")
	}
	if len(it.By) > maxByLen || strings.ContainsAny(it.By, "\n\r") {
		return fmt.Errorf("by: at most %d characters, one line", maxByLen)
	}
	if !(it.Conf >= 0 && it.Conf <= 1) {
		return errors.New("conf must be in [0,1]")
	}
	switch it.Action {
	case "", "confirm", "correct":
		if err := checkLabel(it.Label); err != nil {
			return err
		}
	case "discard":
		if it.Teacher != "" {
			return errors.New("a teacher answers a label; only a person discards a case")
		}
		if it.Label != "" {
			return errors.New("discard takes no label")
		}
	default:
		return fmt.Errorf("action must be confirm, correct or discard, not %q", it.Action)
	}
	if it.Teacher != "" && !teacherRE.MatchString(it.Teacher) {
		return fmt.Errorf("invalid teacher name %q (lowercase letters, digits, . _ -)", it.Teacher)
	}
	return nil
}

func checkLabel(l string) error {
	if l == "" || len(l) > 128 || strings.ContainsAny(l, "\n\r\"\\\x00") || l == Unknown {
		return fmt.Errorf("invalid label %q", l)
	}
	return nil
}

// appendFeedback añade líneas a feedback.jsonl de una vez (O_APPEND) y con
// tope: una etiqueta humana no caduca nunca, así que el fichero no puede
// crecer sin fin; lleno, se dice (507), no se tira nada.
func (l *learner) appendFeedback(task string, lines [][]byte) error {
	l.fbMu.Lock()
	defer l.fbMu.Unlock()
	if err := os.MkdirAll(l.taskDir(task), 0o700); err != nil {
		return err
	}
	p := l.feedbackPath(task)
	var size int64
	if st, err := os.Stat(p); err == nil {
		size = st.Size()
	}
	var all []byte
	for _, b := range lines {
		all = append(all, b...)
	}
	if size+int64(len(all)) > maxFeedbackBytes {
		return statusf(http.StatusInsufficientStorage, "feedback store of task %q is full (%d MiB): retrain and archive it", task, maxFeedbackBytes>>20)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(all)
	return err
}

// ---- captura desde las rutas de decisión

// learnState es lo que el camino de la petición necesita saber de la mejora
// continua de una tarea: con qué ajustes captura y qué versión de Chispa
// sirve. Se calcula al instalar un registro (fuera de los candados) y se lee
// bajo cfgMu, como las cascadas.
type learnState struct {
	cfg     LearnConfig // con los valores por defecto
	version string      // "v3", o "" si el modelo no es ninguna versión conocida
	sha     string
}

func (g *Gateway) learnFor(task string) (learnState, bool) {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	st, ok := g.learnStates[task]
	return st, ok
}

// captureEscalation encola una duda de Chispa. p trae la distribución
// (PredictFull). votes son las respuestas de maestros que ya se tienen.
func (g *Gateway) captureEscalation(st learnState, task, id, kind string, in chispa.Input, p chispa.Prediction, votes []TeacherVote) {
	if !st.cfg.capturing() {
		return
	}
	rec := &CaptureRecord{
		ID: id, Task: task, TS: time.Now().UTC(), Kind: kind, Fields: copyFields(in.Fields),
		Chispa:   CaptureChispa{Version: st.version, SHA256: st.sha, Label: p.Label, Prob: round4(p.Prob), Top: roundTop(topN(p.Probs, learnTopN))},
		Teachers: votes,
	}
	// El texto se copia: el de la petición muere con ella.
	g.learn.enqueue(st.cfg, rec, strings.Clone(in.Text))
}

func copyFields(f map[string]any) map[string]any {
	if len(f) == 0 {
		return nil
	}
	out := make(map[string]any, len(f))
	for k, v := range f {
		out[k] = v
	}
	return out
}

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

func roundTop(ps []chispa.ClassProb) []chispa.ClassProb {
	out := make([]chispa.ClassProb, len(ps))
	for i, c := range ps {
		out[i] = chispa.ClassProb{Label: c.Label, Prob: round4(c.Prob)}
	}
	return out
}
