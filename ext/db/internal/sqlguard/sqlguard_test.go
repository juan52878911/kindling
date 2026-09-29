package sqlguard

import (
	"errors"
	"strings"
	"testing"
)

func TestAceptaConsultas(t *testing.T) {
	for _, sql := range []string{
		"SELECT 1",
		"select count(*) from orders",
		"SELECT c.name, sum(o.total) AS total FROM customers c JOIN orders o ON o.customer_id = c.id GROUP BY 1 ORDER BY 2 DESC",
		"WITH t AS (SELECT * FROM orders WHERE created_at > now() - interval '7 days') SELECT count(*) FROM t",
		"SELECT \"Update\", \"insert\" FROM \"Weird Table\"",
		"SELECT 'DROP TABLE x; DELETE FROM y' AS texto",
		"SELECT name FROM products -- the cheapest\nORDER BY price LIMIT 5",
		"SELECT /* a /* nested */ comment */ 1",
		"SELECT x::numeric(10,2), 1.5e-3, 'it''s' FROM t",
		"SELECT año, straße FROM ventas",
		"SELECT start, \"end\", close FROM prices",
		"SELECT * FROM t FOR SHAREd",
		"SELECT current_setting('TimeZone')",
	} {
		if err := Validate(sql); err != nil {
			t.Errorf("%q: %v", sql, err)
		}
	}
}

// La batería adversaria: todo esto se rechaza.
func TestRechazaBateriaAdversaria(t *testing.T) {
	for _, sql := range []string{
		"",
		"   ",
		"-- only a comment",
		"/* only */",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"DROP TABLE t",
		"TRUNCATE t",
		"ALTER ROLE app SUPERUSER",
		"CREATE TABLE x (a int)",
		"GRANT ALL ON t TO public",
		"COPY t TO PROGRAM 'id'",
		"COPY (SELECT 1) TO PROGRAM 'curl evil'",
		"SET ROLE postgres",
		"SET SESSION AUTHORIZATION postgres",
		"RESET ROLE",
		"BEGIN",
		"COMMIT",
		"DO $$ BEGIN PERFORM 1; END $$",
		"CALL p()",
		"LISTEN x",
		"NOTIFY x",
		"VACUUM t",
		"EXPLAIN ANALYZE DELETE FROM t",
		"TABLE t",
		"VALUES (1)",
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT pg_catalog.pg_read_file('/etc/passwd')",
		"SELECT \"pg_read_file\"('/etc/passwd')",
		"SELECT \"pg_catalog\".\"PG_READ_FILE\"('/etc/passwd')",
		"SELECT pg_read_binary_file('/etc/shadow')",
		"SELECT * FROM pg_ls_dir('/')",
		"SELECT pg_sleep(100)",
		"SELECT pg_sleep_for('1 hour')",
		"SELECT * FROM dblink('host=evil', 'SELECT 1') AS t(a int)",
		"SELECT lo_import('/etc/passwd')",
		"SELECT lo_export(1, '/tmp/x')",
		"SELECT set_config('role', 'postgres', false)",
		"SELECT pg_terminate_backend(1)",
		"SELECT pg_advisory_lock(1)",
		"SELECT nextval('s')",
		"SELECT query_to_xml('DELETE FROM t RETURNING 1', true, true, '')",
		"SELECT pg_notify('c', 'x')",
		"SELECT pg_reload_conf()",
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
		"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x",
		"WITH u AS (UPDATE t SET a = 1 RETURNING a) SELECT * FROM u",
		"SELECT 1; DROP TABLE t",
		"SELECT 1;",
		"SELECT 1 /* ; */ ; DELETE FROM t",
		"SELECT 1 -- \n; DELETE FROM t",
		"SELECT 1 /* unterminated",
		"SELECT 1 /* /* nested */ still open",
		"SELECT 'unterminated",
		"SELECT \"unterminated",
		"SELECT $$ ; DROP TABLE t; $$",
		"SELECT $tag$x$tag$",
		"SELECT $1",
		"SELECT E'\\x41'",
		"SELECT 1 \\! id",
		"SELECT 1\n\\! rm -rf /",
		"SELECT U&\"pg!0073leep\" UESCAPE '!' (1)",
		"SELECT U&'\\0041'",
		"SELECT * INTO nueva FROM t",
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t FOR NO KEY UPDATE",
		"SELECT * FROM t FOR SHARE",
		"SELECT * FROM t FOR KEY SHARE",
		"SELECT * FROM t FOR UPDATE NOWAIT",
		"SELECT 1) q; DELETE FROM t; SELECT (1",
		"SELECT 1) UNION (SELECT 2",
		"SELECT (1",
		"SELECT 1\x00",
		"SELECT 1\x1b[2J",
		"SELECT '\xff'",
		"SELECT '" + strings.Repeat("a", MaxLen) + "'",
	} {
		err := Validate(sql)
		if err == nil {
			t.Errorf("accepted %q", sql)
			continue
		}
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%q: error of type %T", sql, err)
		}
	}
}

func TestExtract(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":               "SELECT 1",
		"SELECT 1;\n":            "SELECT 1",
		"```sql\nSELECT 1;\n```": "SELECT 1",
		"Here it is:\n```postgresql\nSELECT 2\n```": "SELECT 2",
		"```\nSELECT 3\n```\nthanks":                "SELECT 3",
		// solo UN punto y coma final: el resto lo rechaza Validate
		"SELECT 1;;": "SELECT 1;",
	} {
		got, err := Extract(in)
		if err != nil || got != want {
			t.Errorf("Extract(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "```sql\n```", ";"} {
		if _, err := Extract(in); !errors.Is(err, ErrNoSQL) {
			t.Errorf("Extract(%q): %v, want ErrNoSQL", in, err)
		}
	}
}
