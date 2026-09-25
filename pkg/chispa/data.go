package chispa

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Topes de la lectura de JSONL: una línea gigante o un fichero sin fin no
// tumban el proceso.
const (
	MaxLineBytes   = 1 << 20
	MaxDataBytes   = 4 << 30
	DefaultMaxRows = 5_000_000
)

// Example es una línea de JSONL de entrenamiento o evaluación:
//
//	{"text": "...", "label": "...", "fields": {"service": "api", "level": "error"}, "weight": 0.5}
//
// weight (opcional, 0 o ausente = 1) pesa el ejemplo al entrenar: lo usa el
// bucle de mejora continua (docs/mejora-continua.md) para que una etiqueta que
// puso un maestro automático cuente menos que una del conjunto de oro o de un
// humano. No cambia nada al evaluar.
//
// Las claves desconocidas se ignoran (el constructor de datos de commits añade
// "repo" y "time" para poder analizar después sin que el modelo las vea).
type Example struct {
	Text   string         `json:"text"`
	Label  string         `json:"label,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
	Weight float64        `json:"weight,omitempty"`
}

// MaxWeight acota el peso de un ejemplo: uno solo no puede valer por un
// conjunto entero.
const MaxWeight = 100

// Input devuelve la parte que ve el modelo.
func (e Example) Input() Input { return Input{Text: e.Text, Fields: e.Fields} }

// ReadExamples lee JSONL con topes: como mucho MaxDataBytes, líneas de
// MaxLineBytes y maxRows ejemplos (0 = DefaultMaxRows). Si needLabel, una
// línea sin etiqueta es un error con su número de línea.
func ReadExamples(r io.Reader, maxRows int, needLabel bool) ([]Example, error) {
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	sc := bufio.NewScanner(io.LimitReader(r, MaxDataBytes))
	sc.Buffer(make([]byte, 64<<10), MaxLineBytes)
	var out []Example
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		if len(out) == maxRows {
			return nil, fmt.Errorf("more than %d examples", maxRows)
		}
		var ex Example
		if err := json.Unmarshal(b, &ex); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if needLabel && ex.Label == "" {
			return nil, fmt.Errorf("line %d: missing \"label\"", line)
		}
		if !(ex.Weight >= 0 && ex.Weight <= MaxWeight) {
			return nil, fmt.Errorf("line %d: weight must be in [0,%d] (0 = 1)", line, MaxWeight)
		}
		out = append(out, ex)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("line %d: longer than %d bytes", line+1, MaxLineBytes)
		}
		return nil, err
	}
	return out, nil
}
