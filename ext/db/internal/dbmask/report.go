package dbmask

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// Report es el informe final de una construcción. A propósito no tiene ni un
// campo en el que quepa un valor de la base: nombres, tipos de regla,
// acciones y recuentos. El valor de una regla fixed tampoco sale (es del
// usuario, pero el informe se pega en tickets y chats: mejor que no lleve nada
// que parezca un dato).
type Report struct {
	Golden     string          `json:"golden"`
	Source     string          `json:"source"` // usuario@host:puerto/base, sin contraseña
	Tables     int             `json:"tables"`
	Rows       int64           `json:"rows"`
	Masked     []MaskedColumn  `json:"masked"`
	Kept       []string        `json:"kept"`
	Suspicious []SuspiciousCol `json:"suspicious"`
	// Unmasked son las sospechosas que quedaron sin tratar (-allow-unmasked).
	Unmasked []string `json:"unmasked"`
}

// MaskedColumn es una columna tratada.
type MaskedColumn struct {
	Column string `json:"column"` // esquema.tabla.columna
	Kind   Kind   `json:"kind"`
	Rows   int64  `json:"rows"`   // filas de la tabla
	Values int64  `json:"values"` // filas que tenían valor y cambiaron
}

// SuspiciousCol es una columna sospechosa y qué se hizo con ella.
type SuspiciousCol struct {
	Column string `json:"column"`
	Reason string `json:"reason"`
	Action Action `json:"action"`
	Kind   Kind   `json:"kind,omitempty"`
}

// NewReport junta plan y resultado.
func NewReport(golden, source string, p *Plan, r *Result) *Report {
	rep := &Report{Golden: golden, Source: source, Tables: p.TableCount, Rows: p.RowCount,
		Masked: []MaskedColumn{}, Kept: []string{}, Suspicious: []SuspiciousCol{}, Unmasked: []string{}}
	for i, t := range p.Tables {
		for j, u := range t.Updates {
			mc := MaskedColumn{Column: t.Schema + "." + t.Table + "." + u.Column, Kind: u.Kind}
			if r != nil && i < len(r.Rows) {
				mc.Rows = r.Rows[i]
				if j < len(r.Columns[i]) {
					mc.Values = r.Columns[i][j].Masked
				}
			}
			rep.Masked = append(rep.Masked, mc)
		}
	}
	for _, k := range p.Kept {
		rep.Kept = append(rep.Kept, k.Key())
	}
	for _, f := range p.Findings {
		rep.Suspicious = append(rep.Suspicious, SuspiciousCol{Column: f.Key(), Reason: f.Reason, Action: f.Action, Kind: f.Kind})
		if f.Action == ActUnmasked {
			rep.Unmasked = append(rep.Unmasked, f.Key())
		}
	}
	return rep
}

// WriteJSON escribe el informe en JSON.
func (rep *Report) WriteJSON(w io.Writer) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(rep)
}

// WriteText escribe el informe para una persona.
func (rep *Report) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "golden %s from %s: %d tables, %d rows\n", rep.Golden, rep.Source, rep.Tables, rep.Rows)
	fmt.Fprintf(tw, "\nmasked columns (%d):\n", len(rep.Masked))
	for _, m := range rep.Masked {
		fmt.Fprintf(tw, "  %s\t%s\t%d of %d rows\n", m.Column, m.Kind, m.Values, m.Rows)
	}
	if len(rep.Kept) > 0 {
		fmt.Fprintf(tw, "\nkept on purpose (keep rule, %d):\n", len(rep.Kept))
		for _, k := range rep.Kept {
			fmt.Fprintf(tw, "  %s\n", k)
		}
	}
	fmt.Fprintf(tw, "\nsuspicious columns (%d):\n", len(rep.Suspicious))
	for _, s := range rep.Suspicious {
		what := string(s.Action)
		switch s.Action {
		case ActMasked:
			what = "masked (" + string(s.Kind) + ")"
		case ActKept:
			what = "kept (keep rule)"
		case ActUnmasked:
			what = "LEFT UNMASKED (-allow-unmasked)"
		case ActGenerated:
			what = "generated column: recomputed from its inputs"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.Column, s.Reason, what)
	}
	if len(rep.Unmasked) > 0 {
		fmt.Fprintf(tw, "\nWARNING: %d suspicious column(s) were copied as they are\n", len(rep.Unmasked))
	}
	return tw.Flush()
}
