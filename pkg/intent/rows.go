package intent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Row es una orden etiquetada, para evaluar una cascada:
//
//	{"text", "lang", "intent", "slots": {...}}
//
// slots lo interpreta el Domain (ParseSlots); los demás campos de la línea
// se ignoran.
type Row struct {
	Text   string          `json:"text"`
	Lang   string          `json:"lang,omitempty"`
	Intent string          `json:"intent"`
	Slots  json.RawMessage `json:"slots,omitempty"`
}

// Topes de lectura de un JSONL de filas.
const (
	MaxRowBytes = 64 << 10
	MaxRows     = 2_000_000
	MaxDataSize = 1 << 30
)

// ReadRows lee filas JSONL con topes (bytes totales, por línea y número de
// filas). Cada una necesita text e intent.
func ReadRows(r io.Reader) ([]Row, error) {
	sc := bufio.NewScanner(io.LimitReader(r, MaxDataSize))
	sc.Buffer(make([]byte, 64<<10), MaxRowBytes)
	var out []Row
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		if len(out) == MaxRows {
			return nil, fmt.Errorf("more than %d rows", MaxRows)
		}
		var row Row
		if err := json.Unmarshal(b, &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if row.Text == "" || row.Intent == "" {
			return nil, fmt.Errorf("line %d: missing text or intent", line)
		}
		out = append(out, row)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("line %d: longer than %d bytes", line+1, MaxRowBytes)
		}
		return nil, err
	}
	return out, nil
}
