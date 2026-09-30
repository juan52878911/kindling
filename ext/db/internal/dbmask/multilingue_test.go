package dbmask

import (
	"errors"
	"sort"
	"testing"
)

// Lo que la auditoría vio salir tal cual con una sola regla (users.email):
// nombres en español, palabras de identidad en columnas de texto y tipos que
// guardan documentos, lexemas o posiciones. Ahora todo eso para el clon.
func TestPlanSospechosasMultilingueYTipos(t *testing.T) {
	col := func(name, typ, cat string) Column {
		return Column{Schema: "public", Table: "users", Column: name, Type: typ, Category: cat}
	}
	cols := []Column{
		col("id", "integer", "N"),
		col("email", "text", "S"),
		col("correo", "text", "S"),
		col("telefono", "text", "S"),
		col("teléfono_movil", "text", "S"),
		col("nombre", "text", "S"),
		col("domicilio", "text", "S"),
		col("endereco", "text", "S"),
		col("prenom", "text", "S"),
		col("fecha_nacimiento", "date", "D"),
		col("login", "text", "S"),
		col("handle", "character varying(40)", "S"),
		col("notes", "text", "S"),
		col("recipient", "text", "S"),
		col("owner", "text", "S"),
		col("contact", "jsonb", "U"),
		col("search_vector", "tsvector", "U"),
		col("attrs", "hstore", "U"),
		col("location", "point", "G"),
		col("tags", "text[]", "A"),
		// controles: no sospechosos sin -strict
		col("owner_id", "bigint", "N"),
		col("summary", "text", "S"),
		col("total", "numeric", "N"),
		col("scores", "integer[]", "A"),
	}
	cat := &Catalog{Tables: []Table{{Schema: "public", Name: "users", Rows: 1}}, Columns: cols}
	_, err := NewPlan(cat, mustRules(t, "users.email: email\n"), Options{})
	var ue *UnmaskedError
	if !errors.As(err, &ue) {
		t.Fatalf("NewPlan = %v, quería UnmaskedError", err)
	}
	var got []string
	for _, f := range ue.Columns {
		got = append(got, f.Column)
	}
	sort.Strings(got)
	want := []string{"attrs", "contact", "correo", "domicilio", "endereco", "fecha_nacimiento", "handle",
		"location", "login", "nombre", "notes", "owner", "prenom", "recipient", "search_vector", "tags",
		"telefono", "teléfono_movil"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("bloquean %v\nquería    %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("bloquean %v\nquería    %v", got, want)
		}
	}
}

// Con -strict, tsvector, hstore y point también cuentan (antes se escapaban).
func TestStrictTypeCubreLosRaros(t *testing.T) {
	for _, c := range []Column{
		{Type: "tsvector", Category: "U"}, {Type: "hstore", Category: "U"}, {Type: "point", Category: "G"},
		{Type: "geometry(Point,4326)", Category: "U"}, {Type: "text[]", Category: "A"},
	} {
		if !strictType(c) {
			t.Errorf("strictType(%s) = false", c.Type)
		}
	}
}
