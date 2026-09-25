package triage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Feedback es una línea del fichero de confirmaciones: la exportación que
// consume la mejora continua. Es a la vez un ejemplo que `kling chispa train`
// acepta tal cual (text, label, fields; las demás claves las ignora), y lleva
// lo necesario para auditarlo o reentrenar con criterio: qué dijo cada capa,
// con qué confianza, qué líneas se eligieron y el hash del log (nunca el log).
type Feedback struct {
	Schema string         `json:"schema"` // "ci-triage.feedback/v1"
	Text   string         `json:"text"`   // el trozo que vieron Chispa y VON
	Label  string         `json:"label"`  // la categoría que confirmó o puso la persona
	Fields map[string]any `json:"fields,omitempty"`

	Source      string     `json:"source"`    // "human"
	Agreed      bool       `json:"agreed"`    // la persona confirmó lo que salió
	Predicted   string     `json:"predicted"` // la categoría final del triaje
	DecidedBy   string     `json:"decided_by"`
	ChispaLabel string     `json:"chispa_label"`
	ChispaProb  float64    `json:"chispa_prob"`
	ChispaSure  bool       `json:"chispa_confident"`
	VONLabel    string     `json:"von_label,omitempty"`
	Chunks      []ChunkOut `json:"chunks"`
	LogSHA256   string     `json:"log_sha256"`
	LogLines    int        `json:"log_lines"`
	Note        string     `json:"note,omitempty"`
	Time        time.Time  `json:"time"`
}

// FeedbackSchema versiona el formato: quien lo consuma puede rechazar lo que
// no entienda.
const FeedbackSchema = "ci-triage.feedback/v1"

// maxNote acota la nota libre de quien confirma.
const maxNote = 500

// NewFeedback arma la confirmación de un triaje.
func NewFeedback(r *Result, fields map[string]any, logHash, label, note string) (*Feedback, error) {
	if !ValidHumanCategory(label) {
		return nil, fmt.Errorf("unknown category %q", label)
	}
	fb := &Feedback{
		Schema: FeedbackSchema, Text: r.Chunk, Label: label, Fields: fields, Source: "human",
		Agreed: label == r.Category, Predicted: r.Category, DecidedBy: r.Layer,
		ChispaProb: r.Prob, ChispaSure: r.Confident, Chunks: r.Chunks,
		LogSHA256: logHash, LogLines: r.Lines, Note: truncUTF8(note, maxNote), Time: time.Now().UTC(),
	}
	fb.ChispaLabel = r.ChispaLabel
	if r.VON != nil {
		fb.VONLabel = r.VON.Category
	}
	return fb, nil
}

// HashLog es el sha256 de las líneas visibles del log: identifica el log sin
// guardarlo (los logs pueden llevar secretos o datos privados).
func HashLog(lg *Log) string {
	h := sha256.New()
	for _, l := range lg.Lines {
		h.Write([]byte(l.Text))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FeedbackLog añade confirmaciones a un JSONL, una por línea, con un tope de
// tamaño: una página abierta a la red local no puede llenar el disco.
type FeedbackLog struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
}

// NewFeedbackLog abre (o crea, 0600) el fichero de confirmaciones.
func NewFeedbackLog(path string, maxBytes int64) (*FeedbackLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &FeedbackLog{path: path, maxBytes: maxBytes}, nil
}

// ErrFeedbackFull: el fichero llegó a su tope.
var ErrFeedbackFull = errors.New("feedback file is full")

// Append escribe una confirmación entera o nada (una sola escritura con
// O_APPEND bajo el candado).
func (l *FeedbackLog) Append(fb *Feedback) error {
	b, err := json.Marshal(fb)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size()+int64(len(b)) > l.maxBytes {
		return ErrFeedbackFull
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}
