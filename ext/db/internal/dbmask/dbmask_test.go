package dbmask

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func mustRules(t *testing.T, s string) *Rules {
	t.Helper()
	rs, err := ParseRules([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestReglasJSONyYAML(t *testing.T) {
	j := mustRules(t, `{"users.email": "email", "billing.cards.number": "card", "users.bio": "fixed:it's redacted", "users.nick": "keep"}`)
	y := mustRules(t, `
# reglas de prueba
users.email: email
billing.cards.number: card   # la tarjeta
users.bio: "fixed:it's redacted"
'users.nick': keep
`)
	if j.Len() != 4 || y.Len() != 4 {
		t.Fatalf("len json %d yaml %d", j.Len(), y.Len())
	}
	for _, rs := range []*Rules{j, y} {
		r, ok := rs.Get("public", "users", "email")
		if !ok || r.Kind != Email {
			t.Fatalf("users.email: %+v %v", r, ok)
		}
		r, ok = rs.Get("billing", "cards", "number")
		if !ok || r.Kind != Card {
			t.Fatalf("billing.cards.number: %+v %v", r, ok)
		}
		r, _ = rs.Get("public", "users", "bio")
		if r.Kind != Fixed || r.Value != "it's redacted" {
			t.Fatalf("fixed: %+v", r)
		}
	}
}

func TestReglasMalas(t *testing.T) {
	for _, s := range []string{
		`{"users.email": "email", "users.email": "name"}`, // repetida (JSON se la tragaría)
		"users.email: email\nusers.email: keep",           // repetida en YAML
		`{"users.email": "hash"}`,                         // tipo desconocido
		`{"email": "email"}`,                              // sin tabla
		`{"a.b.c.d": "email"}`,                            // demasiadas partes
		`{"users.": "email"}`,                             // nombre vacío
		`{"users.email": 3}`,                              // no es cadena
		`{"users.email": "email"} x`,                      // basura detrás
		"users.email email",                               // sin ':'
		"users.bio: \"fixed:abc",                          // comilla sin cerrar
		`{"users.bio": "fixed:a\nb"}`,                     // control en el valor
		`{"users.e\u0000": "email"}`,                      // control en el nombre
		`{"` + strings.Repeat("x", 64) + `.c": "keep"}`,   // nombre largo
	} {
		if _, err := ParseRules([]byte(s)); err == nil {
			t.Errorf("accepted: %q", s)
		}
	}
}

func TestDeteccion(t *testing.T) {
	cases := map[string]string{
		"email": "email", "customer_email": "email", "EmailAddress": "email", "contact_mail": "email",
		"phone": "phone", "mobile_number": "phone", "tel": "phone",
		"first_name": "name", "lastName": "name", "full_name": "name", "name": "name", "surname": "name",
		"dni": "dni", "tax_nif": "dni", "passport_no": "dni",
		"iban": "iban", "card_number": "card", "cardNumber": "card",
		"address": "address", "billing_address2": "address", "street": "address", "zip": "address",
		"ip": "ip", "last_login_ip": "ip", "date_of_birth": "birth",
		// no sospechosas
		"id": "", "created_at": "", "zipper": "", "description": "", "amount": "", "shipping": "", "tip": "",
		"status": "", "skip": "", "discard": "",
	}
	for col, want := range cases {
		if got := Suspicious(col); got != want {
			t.Errorf("Suspicious(%q) = %q, want %q", col, got, want)
		}
	}
}

func catalogo() *Catalog {
	return &Catalog{
		Tables: []Table{
			{Schema: "public", Name: "users", Rows: 3},
			{Schema: "public", Name: "orders", Rows: 5},
			{Schema: "public", Name: "events", Partitioned: true},
			{Schema: "public", Name: "events_2026", Root: "public.events", Rows: 7},
		},
		Columns: []Column{
			{Schema: "public", Table: "users", Column: "id", Type: "integer", Category: "N"},
			{Schema: "public", Table: "users", Column: "email", Type: "text", Category: "S"},
			{Schema: "public", Table: "users", Column: "full_name", Type: "character varying(80)", Category: "S"},
			{Schema: "public", Table: "users", Column: "display", Type: "text", Category: "S", Generated: true},
			{Schema: "public", Table: "users", Column: "notes", Type: "text", Category: "S"},
			{Schema: "public", Table: "users", Column: "last_ip", Type: "inet", Category: "I"},
			{Schema: "public", Table: "orders", Column: "id", Type: "integer", Category: "N"},
			{Schema: "public", Table: "orders", Column: "customer_email", Type: "text", Category: "S"},
			{Schema: "public", Table: "orders", Column: "phone", Type: "bigint", Category: "N"},
			{Schema: "public", Table: "orders", Column: "origin", Type: "inet", Category: "I"},
			{Schema: "public", Table: "events", Column: "email", Type: "text", Category: "S"},
			{Schema: "public", Table: "events_2026", Column: "email", Type: "text", Category: "S"},
		},
	}
}

const reglasBuenas = `
users.email: email
users.full_name: name
users.last_ip: "null"
orders.customer_email: email
orders.phone: phone
orders.origin: "fixed:10.0.0.1"
events.email: email
`

func TestPlanBloqueaSospechosasSinRegla(t *testing.T) {
	rs := mustRules(t, "users.email: email\n")
	_, err := NewPlan(catalogo(), rs, Options{})
	var ue *UnmaskedError
	if !errors.As(err, &ue) || !errors.Is(err, ErrUnmasked) {
		t.Fatalf("want UnmaskedError, got %v", err)
	}
	got := map[string]bool{}
	for _, f := range ue.Columns {
		got[f.Key()] = true
	}
	for _, k := range []string{"public.users.full_name", "public.users.last_ip", "public.orders.customer_email", "public.orders.phone", "public.orders.origin", "public.events.email"} {
		if !got[k] {
			t.Errorf("%s should block; got %v", k, got)
		}
	}
	// La partición la cubre su raíz; la generada se recalcula; notes no dice nada.
	for _, k := range []string{"public.events_2026.email", "public.users.display", "public.users.notes", "public.users.email"} {
		if got[k] {
			t.Errorf("%s should not block", k)
		}
	}

	p, err := NewPlan(catalogo(), rs, Options{AllowUnmasked: true})
	if err != nil {
		t.Fatal(err)
	}
	rep := NewReport("g", "ro@db:5432/app", p, nil)
	if len(rep.Unmasked) != 6 {
		t.Fatalf("unmasked in report: %v", rep.Unmasked)
	}
}

func TestPlanStrict(t *testing.T) {
	rs := mustRules(t, reglasBuenas)
	if _, err := NewPlan(catalogo(), rs, Options{}); err != nil {
		t.Fatal(err)
	}
	_, err := NewPlan(catalogo(), rs, Options{Strict: true})
	var ue *UnmaskedError
	if !errors.As(err, &ue) || len(ue.Columns) != 1 || ue.Columns[0].Key() != "public.users.notes" {
		t.Fatalf("strict should flag only users.notes: %v", err)
	}
	rs = mustRules(t, reglasBuenas+"users.notes: text\n")
	if _, err := NewPlan(catalogo(), rs, Options{Strict: true}); err != nil {
		t.Fatal(err)
	}
}

func TestPlanReglasQueNoCasan(t *testing.T) {
	for _, extra := range []string{
		"users.emial: email",       // errata: no existe
		"events_2026.email: email", // partición: a la raíz
		"users.display: text",      // generada
		"nope.users.email: keep",   // esquema inexistente
	} {
		rs := mustRules(t, reglasBuenas+extra+"\n")
		if _, err := NewPlan(catalogo(), rs, Options{AllowUnmasked: true}); err == nil {
			t.Errorf("%s: accepted", extra)
		}
	}
	// keep en una generada vale.
	rs := mustRules(t, reglasBuenas+"users.display: keep\n")
	if _, err := NewPlan(catalogo(), rs, Options{}); err != nil {
		t.Fatal(err)
	}
}

const salA = "1111111111111111111111111111111111111111111111111111111111111111"
const salB = "2222222222222222222222222222222222222222222222222222222222222222"

func planBueno(t *testing.T) *Plan {
	t.Helper()
	p, err := NewPlan(catalogo(), mustRules(t, reglasBuenas), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSQLDeterminista(t *testing.T) {
	p := planBueno(t)
	a1, err := MaskSQL(p, salA)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := MaskSQL(planBueno(t), salA)
	b, _ := MaskSQL(p, salB)
	if a1 != a2 {
		t.Fatal("same plan and salt, different SQL")
	}
	if a1 == b || !strings.Contains(b, salB) || strings.Contains(b, salA) {
		t.Fatal("the salt must change the SQL")
	}
	// La expresión de un correo no depende de la tabla ni de la columna: el
	// mismo valor da el mismo resultado en users.email y en orders.customer_email.
	u := Update{Kind: Email, Type: "text", Category: "S"}
	if setExpr(u, salA, "o.kc_0") != setExpr(Update{Kind: Email, Type: "text", Category: "S", Column: "otra"}, salA, "o.kc_0") {
		t.Fatal("email expression depends on the column")
	}
	if !strings.Contains(a1, "session_replication_role = replica") || !strings.HasPrefix(a1, "-- "+MaskMarker) {
		t.Fatal("missing replica role or marker")
	}
	if strings.Count(a1, "UPDATE ") != 3 { // users, orders, events
		t.Fatalf("want 3 UPDATEs:\n%s", a1)
	}
	if !strings.Contains(a1, `UPDATE "public"."events" AS t`) || !strings.Contains(a1, `UPDATE ONLY "public"."users" AS t`) {
		t.Fatalf("ONLY for plain tables, not for partitioned ones:\n%s", a1)
	}
	if !strings.Contains(a1, "'1555' || left(translate(") {
		t.Fatal("a numeric phone column gets digits only")
	}
	if !strings.Contains(a1, `"last_ip" = NULL`) {
		t.Fatal("null rule")
	}
	if !strings.Contains(a1, "1 / (CASE WHEN EXISTS") || !strings.HasSuffix(a1, "COMMIT;\n") {
		t.Fatal("the unchanged check must abort before COMMIT")
	}
	if _, err := MaskSQL(p, "x'; DROP TABLE users; --"); err == nil {
		t.Fatal("a bad salt must be refused")
	}
}

func TestIdentificadoresCitados(t *testing.T) {
	cat := &Catalog{
		Tables:  []Table{{Schema: "public", Name: `we"ird`}},
		Columns: []Column{{Schema: "public", Table: `we"ird`, Column: `e"mail`, Type: "text", Category: "S"}},
	}
	p, err := NewPlan(cat, mustRules(t, `{"we\"ird.e\"mail": "fixed:o'hara"}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := MaskSQL(p, salA)
	if !strings.Contains(sql, `"we""ird"`) || !strings.Contains(sql, `"e""mail"`) || !strings.Contains(sql, `'o''hara'`) {
		t.Fatalf("bad quoting:\n%s", sql)
	}
}

func TestResultadoEInforme(t *testing.T) {
	p := planBueno(t)
	out := "SET\nkc|0|7|7|0\nkc|1|5|4,1,5|0,0,0\nkc|2|3|3,2,0|0,0,0\n"
	// El orden de p.Tables es events, orders, users.
	r, err := ParseResult(p, []byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unchanged(p)) != 0 {
		t.Fatal("nothing unchanged")
	}
	bad, err := ParseResult(p, []byte("kc|0|7|7|0\nkc|1|5|4,1,5|0,0,2\nkc|2|3|3,2,0|0,0,0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if u := bad.Unchanged(p); len(u) != 1 || u[0] != "public.orders.phone" {
		t.Fatalf("unchanged: %v", u)
	}
	if _, err := ParseResult(p, []byte("kc|0|7|7|0\n")); err == nil {
		t.Fatal("a missing table must be an error")
	}

	rep := NewReport("prod-masked", "ro@db.example.com:5432/app", p, r)
	var txt, js bytes.Buffer
	if err := rep.WriteText(&txt); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{txt.String(), js.String()} {
		// Ni el valor fijo, ni la sal.
		for _, secret := range []string{"10.0.0.1", salA} {
			if strings.Contains(s, secret) {
				t.Fatalf("report leaks %q:\n%s", secret, s)
			}
		}
		for _, want := range []string{"public.users.email", "public.orders.phone", "public.users.last_ip"} {
			if !strings.Contains(s, want) {
				t.Fatalf("report lacks %s:\n%s", want, s)
			}
		}
	}
	if !strings.Contains(txt.String(), "4 of 5 rows") {
		t.Fatalf("counts:\n%s", txt.String())
	}
}

func TestCatalogo(t *testing.T) {
	c, err := ParseCatalog([]byte(`{"tables":[{"schema":"public","name":"users","partitioned":false,"root":null,"rows":3}],` +
		`"columns":[{"schema":"public","table":"users","column":"email","type":"text","category":"S","generated":false}]}` + "\n"))
	if err != nil || len(c.Tables) != 1 || c.Columns[0].Column != "email" || c.Tables[0].Rows != 3 {
		t.Fatalf("%+v %v", c, err)
	}
	if !strings.Contains(CatalogSQL, CatalogMarker) {
		t.Fatal("marker")
	}
	if _, err := ParseCatalog([]byte("ERROR")); err == nil {
		t.Fatal("garbage accepted")
	}
}
