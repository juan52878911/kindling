package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Mapeo de MASSIVE a la taxonomía. Lo que no está aquí es out_of_scope: la
// cafetera y el robot aspirador (iot_coffee, iot_cleaning) no están en la
// habitación, y alarm_* de MASSIVE es el despertador («despiértame a las
// siete»), no la alarma de seguridad; son justo los casos que el clasificador
// tiene que saber rechazar.
var massiveMap = map[string]struct{ intent, device string }{
	"iot_hue_lighton":     {"turn_on", domotica.DevLight},
	"iot_hue_lightoff":    {"turn_off", domotica.DevLight},
	"iot_hue_lightup":     {"brightness_up", domotica.DevLight},
	"iot_hue_lightdim":    {"brightness_down", domotica.DevLight},
	"iot_hue_lightchange": {"set_color", domotica.DevLight},
	"iot_wemo_on":         {"turn_on", domotica.DevPlug},
	"iot_wemo_off":        {"turn_off", domotica.DevPlug},
	"audio_volume_up":     {"volume_up", ""},
	"audio_volume_down":   {"volume_down", ""},
	"audio_volume_mute":   {"volume_mute", ""},
	"audio_volume_other":  {"volume_set", ""},
}

// Huecos de MASSIVE que pasan al esquema; el resto (time, date, song_name…)
// se ignoran.
var massiveSlots = map[string]string{
	"house_place":   domotica.SlotArea,
	"device_type":   domotica.SlotDevice,
	"color_type":    domotica.SlotColor,
	"change_amount": domotica.SlotValue,
}

type massiveRow struct {
	ID        string `json:"id"`
	Locale    string `json:"locale"`
	Partition string `json:"partition"`
	Intent    string `json:"intent"`
	Utt       string `json:"utt"`
	AnnotUtt  string `json:"annot_utt"`
}

// parseAnnot reconstruye el texto de «apaga la [house_place : cocina]» y
// devuelve los huecos con su posición en bytes.
func parseAnnot(a string) (string, []slots.Span, error) {
	var b strings.Builder
	var spans []slots.Span
	for i := 0; i < len(a); {
		if a[i] != '[' {
			b.WriteByte(a[i])
			i++
			continue
		}
		j := strings.IndexByte(a[i:], ']')
		if j < 0 {
			return "", nil, fmt.Errorf("unbalanced annotation %q", a)
		}
		inner := a[i+1 : i+j]
		typ, val, ok := strings.Cut(inner, " : ")
		if !ok {
			return "", nil, fmt.Errorf("bad annotation %q", inner)
		}
		val = strings.TrimSpace(val)
		st := b.Len()
		b.WriteString(val)
		spans = append(spans, slots.Span{Slot: strings.TrimSpace(typ), Start: st, End: b.Len()})
		i += j + 1
	}
	return b.String(), spans, nil
}

// valueSpan recorta el hueco change_amount al número que contiene («a
// treinta y cinco por ciento» → «treinta y cinco»): el resto son
// preposiciones y unidades, que se leen aparte. Sin número (un poco, a bit)
// no hay valor.
func valueSpan(text string, sp slots.Span) (slots.Span, bool) {
	var tk slots.Tokenizer
	sub := text[sp.Start:sp.End]
	tk.Run(sub)
	for i := 0; i < tk.Len(); i++ {
		if _, n, ok := domotica.ParseNumberAt(&tk, i); ok {
			s, _ := tk.Pos(i)
			_, e := tk.Pos(i + n - 1)
			return slots.Span{Slot: domotica.SlotValue, Start: sp.Start + s, End: sp.Start + e}, true
		}
	}
	return slots.Span{}, false
}

func loadMassive(files map[string][]byte) ([]domotica.Row, error) {
	var out []domotica.Row
	for _, loc := range []string{"es-ES", "en-US"} {
		data, ok := files["1.0/data/"+loc+".jsonl"]
		if !ok {
			return nil, fmt.Errorf("massive: %s missing from archive", loc)
		}
		lang := loc[:2]
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var m massiveRow
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				return nil, err
			}
			text, raw, err := parseAnnot(m.AnnotUtt)
			if err != nil {
				return nil, err
			}
			// Si alguna fila no reconstruye igual que utt (espacios), manda la
			// anotación: es la que tiene las posiciones de los huecos.
			split := map[string]string{"train": "train", "dev": "valid", "test": "test"}[m.Partition]
			r := domotica.Row{Text: text, Lang: lang, Intent: domotica.OutOfScope, Source: "massive",
				Family: "massive/" + m.ID, Split: split}
			mp, inScope := massiveMap[m.Intent]
			if inScope {
				r.Intent = mp.intent
				for _, sp := range raw {
					name, ok := massiveSlots[sp.Slot]
					if !ok {
						continue
					}
					sp.Slot = name
					if name == domotica.SlotValue {
						if sp, ok = valueSpan(text, sp); !ok {
							continue
						}
					}
					if name == domotica.SlotDevice {
						if _, known := domotica.CanonDevice(text[sp.Start:sp.End]); !known {
							continue // «cafetera», «robot»: no es un dispositivo de la habitación
						}
					}
					r.Spans = append(r.Spans, sp)
				}
			}
			finishRow(&r, mp.device)
			out = append(out, r)
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
