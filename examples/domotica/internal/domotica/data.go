package domotica

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Slots son los huecos de una orden, ya normalizados: dispositivo y zona
// canónicos, valor numérico con su unidad, color canónico.
type Slots struct {
	Device   string
	Area     string
	Value    float64
	HasValue bool
	Unit     string
	Color    string
}

type slotsJSON struct {
	Device string   `json:"device,omitempty"`
	Area   string   `json:"area,omitempty"`
	Value  *float64 `json:"value,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Color  string   `json:"color,omitempty"`
}

// MarshalJSON: {"device","area","value","unit","color"}, sin los vacíos.
func (s Slots) MarshalJSON() ([]byte, error) {
	j := slotsJSON{Device: s.Device, Area: s.Area, Unit: s.Unit, Color: s.Color}
	if s.HasValue {
		v := s.Value
		j.Value = &v
	}
	return json.Marshal(j)
}

// UnmarshalJSON es la inversa de MarshalJSON.
func (s *Slots) UnmarshalJSON(b []byte) error {
	var j slotsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*s = Slots{Device: j.Device, Area: j.Area, Unit: j.Unit, Color: j.Color}
	if j.Value != nil {
		s.Value, s.HasValue = *j.Value, true
	}
	return nil
}

// Pairs devuelve los huecos presentes como pares nombre=valor (para el F1 de
// valores).
func (s Slots) Pairs() []string {
	var out []string
	if s.Device != "" {
		out = append(out, "device="+s.Device)
	}
	if s.Area != "" {
		out = append(out, "area="+s.Area)
	}
	if s.HasValue {
		out = append(out, "value="+FormatValue(s.Value))
	}
	if s.Unit != "" {
		out = append(out, "unit="+s.Unit)
	}
	if s.Color != "" {
		out = append(out, "color="+s.Color)
	}
	return out
}

// Row es una línea del conjunto unificado:
//
//	{"text", "lang", "intent", "slots": {...}, "spans": [...], "source", "family", "split"}
//
// spans son los huecos en bytes del texto (lo que aprende el etiquetador);
// slots, sus valores normalizados (lo que se compara al evaluar). family agrupa
// las frases que no deben cruzar de un reparto a otro.
type Row struct {
	Text   string       `json:"text"`
	Lang   string       `json:"lang"`
	Intent string       `json:"intent"`
	Slots  Slots        `json:"slots"`
	Spans  []slots.Span `json:"spans,omitempty"`
	Source string       `json:"source"`
	Family string       `json:"family,omitempty"`
	Split  string       `json:"split,omitempty"`
	// Class es la clase de error de las frases de reto (indirect, multi,
	// oos_near…); vacía en los datos normales.
	Class string `json:"class,omitempty"`
}

// Topes de lectura del JSONL unificado.
const (
	MaxRowBytes = 64 << 10
	MaxRows     = 2_000_000
	MaxDataSize = 1 << 30
)

// ReadRows lee JSONL con topes (bytes totales, por línea y número de filas).
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
		for _, sp := range row.Spans {
			if sp.Start < 0 || sp.End > len(row.Text) || sp.Start >= sp.End {
				return nil, fmt.Errorf("line %d: span out of range", line)
			}
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

// SlotsFromSpans normaliza los huecos encontrados en text: zona y dispositivo
// canónicos, número y unidad, color. Si hay varios del mismo tipo gana el
// primero (las órdenes múltiples escalan; ver Decide).
func SlotsFromSpans(text string, spans []slots.Span) Slots {
	var s Slots
	for _, sp := range spans {
		t := text[sp.Start:sp.End]
		switch sp.Slot {
		case SlotDevice:
			if s.Device == "" {
				if d, ok := CanonDevice(t); ok {
					s.Device = d
				}
			}
		case SlotArea:
			if s.Area == "" {
				s.Area, _ = CanonArea(t)
			}
		case SlotColor:
			if s.Color == "" {
				s.Color, _ = CanonColor(t)
			}
		case SlotValue:
			if !s.HasValue {
				if v, ok := ParseValue(t); ok {
					s.Value, s.HasValue = v, true
					s.Unit = unitAround(text, sp)
				}
			}
		}
	}
	return s
}

// unitAround busca la unidad desde el último token del valor: «22 grados»,
// «50 %», «cincuenta por ciento».
func unitAround(text string, sp slots.Span) string {
	var tk slots.Tokenizer
	tk.Run(text)
	last := -1
	for i := 0; i < tk.Len(); i++ {
		if st, _ := tk.Pos(i); st >= sp.Start && st < sp.End {
			last = i
		}
	}
	if last < 0 {
		return ""
	}
	return UnitAfter(&tk, last)
}
