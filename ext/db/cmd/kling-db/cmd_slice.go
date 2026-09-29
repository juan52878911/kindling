package main

// kling db slice: un golden con UNA tabla de producción, un subconjunto
// acotado de sus filas y las filas directamente relacionadas (padres por sus
// claves foráneas, hijos que la apuntan), y el resto del esquema como tablas
// vacías. Es kling db clone (cmd_clone.go) con otro relleno: la misma máquina
// de construcción con -egress allowlist, la misma credencial en el proxy (la
// contraseña de producción no entra en la microVM), el mismo Postgres de
// preparación en un tmpfs, el mismo enmascarado y el mismo golden construido
// en una máquina nueva sin red desde el volcado ya enmascarado.
//
// Lo que cambia es cómo se llena la preparación, y todo es de solo lectura en
// producción:
//
//  1. pg_dump --schema-only: el esquema entero (tablas, índices, claves
//     foráneas, funciones...), sin una fila.
//  2. Una consulta del catálogo de producción dice qué columnas copiar (sin
//     las generadas), la clave primaria de la tabla (para un orden estable) y
//     sus claves foráneas directas, en los dos sentidos.
//  3. Una sola sesión de psql en producción, en una transacción REPEATABLE
//     READ READ ONLY (una única foto para todo), saca con \copy ... TO PROGRAM
//     la muestra (ORDER BY clave primaria LIMIT -rows), los padres que esa
//     muestra apunta y como mucho -related-rows hijos por clave foránea. Cada
//     \copy va por una tubería a un psql de la preparación (COPY FROM STDIN
//     con session_replication_role = replica: ni triggers ni comprobaciones de
//     claves foráneas al cargar). Nada toca un fichero.
//  4. En la preparación: las claves foráneas que las filas cargadas no
//     cumplen (un padre de un padre que no se copió) quedan NOT VALID —se
//     siguen comprobando en lo que se escriba después— o, si Postgres no lo
//     admite (tabla particionada antes de 18), se quitan; el informe dice
//     cuáles. Las secuencias se ponen tras el máximo copiado.
//
// Luego, igual que clone: enmascarado, volcado enmascarado, golden.
//
// El modo de observación (kling db observe, cmd_observe.go) registra en una
// copia las sentencias que tocan la tabla. Los tiempos de una copia así NO son
// los de producción: pocas filas y otras estadísticas llevan a otros planes.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/dbmask"
	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	sliceDefaultRows = 1000
	sliceMaxRows     = 1_000_000
	// sliceMaxRelations acota las claves foráneas que se siguen (y con ello el
	// tamaño del guion).
	sliceMaxRelations = 200
	// sliceFile es el fichero, junto a la contraseña del golden en el host,
	// que dice de qué tabla es el slice (lo lee kling db observe).
	sliceFile = "slice.json"

	sliceDiscoverMarker = "kling-db:slice-discover"
	sliceFillMarker     = "kling-db:slice-fill"
	sliceFixupMarker    = "kling-db:slice-fixup"
)

// sliceTablePattern: la tabla de -table, [esquema.]nombre sin comillas. Un
// nombre que necesite comillas no se admite aquí (sí en las tablas
// relacionadas, que vienen del catálogo ya citadas).
var sliceTablePattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_$]{0,62}\.)?[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

// statsWarning es el aviso que acompaña a slice y al informe de observación.
const statsWarning = "timings from a slice do not represent production: it holds a few rows and its own planner statistics, so plans and durations differ. " +
	"Real statistics can only be imported from PostgreSQL 18 on (pg_restore_relation_stats, pg_restore_attribute_stats); the golden runs Postgres 16."

// sliceSpec es lo que pide -table / -rows / -related-rows.
type sliceSpec struct {
	table   string
	rows    int
	related int
}

// sliceTable es una tabla tal y como la describe el catálogo de producción:
// todo ya citado con quote_ident.
type sliceTable struct {
	relkind string // r o p
	qname   string // "esquema"."tabla"
	plain   string // esquema.tabla, para el informe
	cols    string // "a","b": las columnas no generadas, en orden
	pk      string // "id": la clave primaria (solo la tabla del slice)
}

// from es la tabla en un FROM: ONLY para una tabla normal (sin las hijas de
// herencia, que son otras tablas del esquema), entera si es particionada.
func (t sliceTable) from() string {
	if t.relkind == "r" {
		return "ONLY " + t.qname
	}
	return t.qname
}

// sliceRel es una clave foránea directa: parent (la tabla del slice apunta a
// other) o child (other apunta a la tabla del slice). fk son las columnas del
// lado que apunta y ref las del apuntado.
type sliceRel struct {
	dir     string
	other   sliceTable
	fk, ref string
}

// sliceCount es lo que se cargó de una tabla.
type sliceCount struct {
	Table string `json:"table"`
	Role  string `json:"role"` // slice, parent, child o parent+child
	Rows  int64  `json:"rows"`
}

// sliceResult es la parte del informe propia del slice.
type sliceResult struct {
	Table       string       `json:"table"`
	RowsLimit   int          `json:"rows_limit"`
	RelatedMax  int          `json:"related_rows_limit"`
	Loaded      []sliceCount `json:"loaded"`
	NotValidFKs []string     `json:"not_valid_fks"`
	DroppedFKs  []string     `json:"dropped_fks"`
	Warning     string       `json:"warning"`
}

func cmdSlice(args []string) error {
	fs := flag.NewFlagSet("db slice", flag.ContinueOnError)
	f := addCloneFlags(fs)
	table := fs.String("table", "", "the table to copy, [schema.]name (required)")
	rows := fs.Int("rows", sliceDefaultRows, "rows of the table to copy, in primary key order")
	related := fs.Int("related-rows", 0, "at most this many rows of each table that points to it (default: -rows)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *f.mask == "" || *table == "" {
		return usageErr("usage: kling db slice <postgres-url> -table [schema.]name -mask RULES [-rows N] [-related-rows N] [-golden NAME] [-password-stdin] [-ca FILE]")
	}
	spec := &sliceSpec{table: *table, rows: *rows, related: *related}
	if err := spec.validate(); err != nil {
		return usageErr("%v", err)
	}
	o, err := f.opts(pos[0])
	if err != nil {
		return err
	}
	o.slice = spec
	if o.golden == "" {
		o.golden = defaultSliceGolden(o.source.db, spec.table)
	}
	c, err := f.newCloner()
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	rep, err := c.run(ctx, o)
	if err != nil {
		return err
	}
	if err := writeSliceMeta(o.golden, o.source.String(), c.sliceRes); err != nil {
		// El golden está hecho: sin el fichero, observe pedirá -table.
		c.logf("warning: could not record the slice table for kling db observe: %v", err)
	}
	return writeSliceReport(c.a.stdout, rep, c.sliceRes, o.jsonOut)
}

func (s *sliceSpec) validate() error {
	if !sliceTablePattern.MatchString(s.table) {
		return fmt.Errorf("invalid -table %q: [schema.]name, letters, digits, _ and $ (a name that needs quotes is not supported)", dbmaskSafe(s.table))
	}
	if s.rows < 1 || s.rows > sliceMaxRows {
		return fmt.Errorf("-rows must be 1-%d", sliceMaxRows)
	}
	if s.related == 0 {
		s.related = s.rows
	}
	if s.related < 1 || s.related > sliceMaxRows {
		return fmt.Errorf("-related-rows must be 1-%d", sliceMaxRows)
	}
	return nil
}

// defaultSliceGolden: <base>-<tabla>-slice, dentro de goldenNamePattern.
func defaultSliceGolden(db, table string) string {
	_, name, _ := strings.Cut(table, ".")
	if name == "" {
		name = table
	}
	n := strings.TrimSuffix(defaultGoldenName(db+"-"+name), "-masked")
	if n == "clone" {
		n = "table"
	}
	if len(n) > 34 {
		n = strings.Trim(n[:34], "-_")
	}
	return n + "-slice"
}

// ── relleno ─────────────────────────────────────────────────────────────────

// sliceFill llena el Postgres de preparación de la máquina de construcción:
// esquema, muestra y filas relacionadas, y el arreglo de claves foráneas y
// secuencias. Solo lee de producción.
func (c *cloner) sliceFill(ctx context.Context, builder string, o cloneOpts) (*sliceResult, error) {
	s := o.slice
	c.logf("reading the catalog of %s for %s", o.source, s.table)
	out, err := c.a.k.Run(ctx, strings.NewReader(sliceDiscoverScript(o.source, s.table)), "exec", "-i", "-timeout", "5m", builder, "--", "sh", "-s")
	if err != nil {
		return nil, fmt.Errorf("reading the catalog of the source: %w", err)
	}
	target, rels, err := parseSliceDiscover(out)
	if err != nil {
		return nil, err
	}
	loads := planSlice(target, rels, s)
	script := sliceFillScript(o.source, loads)
	if len(script) > api.ExecMaxStdin {
		return nil, fmt.Errorf("the slice script is over %d bytes (too many related tables)", api.ExecMaxStdin)
	}
	c.logf("copying the schema and up to %d row(s) of %s with %d related table(s) (inside the microVM; nothing on the host's disk)",
		s.rows, target.plain, len(loads)-1)
	if _, err := c.a.k.Run(ctx, strings.NewReader(script), "exec", "-i", "-timeout", "1h", builder, "--", "sh", "-s"); err != nil {
		return nil, fmt.Errorf("copying the slice: %w", err)
	}
	out, err = c.stagingPsql(ctx, builder, sliceFixupSQL(loads), "30m")
	if err != nil {
		return nil, fmt.Errorf("checking the foreign keys of the slice: %w", err)
	}
	res, err := parseSliceFixup(out, loads)
	if err != nil {
		return nil, err
	}
	res.Table, res.RowsLimit, res.RelatedMax, res.Warning = target.plain, s.rows, s.related, statsWarning
	for _, fk := range res.NotValidFKs {
		c.logf("foreign key %s is NOT VALID in the slice (it points to rows that were not copied)", fk)
	}
	for _, fk := range res.DroppedFKs {
		c.logf("foreign key %s was dropped in the slice (NOT VALID is not possible on it)", fk)
	}
	return res, nil
}

// sliceDiscoverScript lee del catálogo de producción la tabla y sus claves
// foráneas directas. Una línea por registro, campos separados por 0x1f. Solo
// lee, en una transacción de solo lectura.
func sliceDiscoverScript(s *cloneSource, table string) string {
	// table pasó sliceTablePattern: sin comillas que escapar.
	return "# " + sliceDiscoverMarker + "\nset -eu\n" + cloneMarkerSnippet +
		"U=" + shq(s.user) + "\nD=" + shq(s.db) + "\n" +
		`psql -X -q -At -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -h ` + cloneDomain + ` -p 5432 -U "$U" -d "$D" <<'SQL'
BEGIN ISOLATION LEVEL REPEATABLE READ, READ ONLY;
WITH tgt AS (SELECT to_regclass('` + table + `') AS oid),
fks AS (
  SELECT k.conrelid, k.confrelid,
    (SELECT string_agg(quote_ident(a.attname), ',' ORDER BY u.i)
       FROM unnest(k.conkey) WITH ORDINALITY u(attnum, i)
       JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = u.attnum) AS fk,
    (SELECT string_agg(quote_ident(a.attname), ',' ORDER BY u.i)
       FROM unnest(k.confkey) WITH ORDINALITY u(attnum, i)
       JOIN pg_attribute a ON a.attrelid = k.confrelid AND a.attnum = u.attnum) AS ref
  FROM pg_constraint k, tgt
  WHERE k.contype = 'f' AND k.conparentid = 0 AND k.conrelid <> k.confrelid
    AND (k.conrelid = tgt.oid OR k.confrelid = tgt.oid)
),
rels AS (
  SELECT c.oid, c.relkind, format('%I.%I', n.nspname, c.relname) AS qname,
    n.nspname || '.' || c.relname AS plain,
    (SELECT string_agg(quote_ident(a.attname), ',' ORDER BY a.attnum) FROM pg_attribute a
      WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = '') AS cols,
    (SELECT string_agg(quote_ident(a.attname), ',' ORDER BY k.i) FROM pg_index x
       CROSS JOIN LATERAL unnest(x.indkey) WITH ORDINALITY AS k(attnum, i)
       JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
      WHERE x.indrelid = c.oid AND x.indisprimary) AS pk
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE c.oid IN (SELECT oid::oid FROM tgt UNION SELECT conrelid FROM fks UNION SELECT confrelid FROM fks)
)
SELECT concat_ws(chr(31), 'target', r.relkind, r.qname, r.plain, coalesce(r.cols, ''), coalesce(r.pk, ''))
  FROM rels r JOIN tgt ON r.oid = tgt.oid
UNION ALL
SELECT concat_ws(chr(31), CASE WHEN f.conrelid = tgt.oid THEN 'parent' ELSE 'child' END,
    r.relkind, r.qname, r.plain, coalesce(r.cols, ''), f.fk, f.ref)
  FROM fks f CROSS JOIN tgt
  JOIN rels r ON r.oid = CASE WHEN f.conrelid = tgt.oid THEN f.confrelid ELSE f.conrelid END;
COMMIT;
SQL
`
}

// nombreSeguro: un nombre del catálogo de producción no puede llevar
// caracteres de control (romperían una línea de \copy o de la terminal) ni
// barras invertidas (psql las interpreta en sus metacomandos).
func nombreSeguro(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	return s != ""
}

// parseSliceDiscover lee la salida de sliceDiscoverScript.
func parseSliceDiscover(out []byte) (sliceTable, []sliceRel, error) {
	var target sliceTable
	var rels []sliceRel
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || !strings.ContainsRune(line, 0x1f) {
			continue
		}
		f := strings.Split(line, "\x1f")
		for _, v := range f[1:] {
			if v != "" && !nombreSeguro(v) {
				return sliceTable{}, nil, errors.New("the source has a table or column name with control characters or backslashes near the slice: not supported")
			}
		}
		switch {
		case f[0] == "target" && len(f) == 6:
			if found {
				return sliceTable{}, nil, errors.New("unexpected answer from the source catalog")
			}
			found = true
			target = sliceTable{relkind: f[1], qname: f[2], plain: f[3], cols: f[4], pk: f[5]}
		case (f[0] == "parent" || f[0] == "child") && len(f) == 7:
			if f[5] == "" || f[6] == "" {
				return sliceTable{}, nil, errors.New("unexpected answer from the source catalog")
			}
			rels = append(rels, sliceRel{dir: f[0], other: sliceTable{relkind: f[1], qname: f[2], plain: f[3], cols: f[4]}, fk: f[5], ref: f[6]})
		default:
			return sliceTable{}, nil, errors.New("unexpected answer from the source catalog")
		}
	}
	if !found {
		return sliceTable{}, nil, errors.New("the table does not exist in the source, or the role cannot see it")
	}
	if target.relkind != "r" && target.relkind != "p" {
		return sliceTable{}, nil, fmt.Errorf("%s is not a table (only ordinary and partitioned tables can be sliced)", dbmaskSafe(target.plain))
	}
	if target.cols == "" {
		return sliceTable{}, nil, fmt.Errorf("%s has no columns to copy", dbmaskSafe(target.plain))
	}
	if len(rels) > sliceMaxRelations {
		return sliceTable{}, nil, fmt.Errorf("%s has %d foreign keys in and out; the limit is %d", dbmaskSafe(target.plain), len(rels), sliceMaxRelations)
	}
	return target, rels, nil
}

// sliceLoad es una tabla a cargar: su consulta en producción (una línea) y su
// \copy en la preparación.
type sliceLoad struct {
	table sliceTable
	role  string
	query string
}

// planSlice arma las consultas. La muestra es la misma en todas (mismo orden
// estable y misma foto): ORDER BY la clave primaria o, sin ella, ctid (y
// tableoid en una particionada), que es único dentro de la foto. Las tablas
// relacionadas se agrupan: una tabla que es padre por dos claves, o padre e
// hija a la vez, se carga una vez, con sus filas elegidas por (tableoid,
// ctid) sin repetir.
func planSlice(t sliceTable, rels []sliceRel, s *sliceSpec) []sliceLoad {
	order := t.pk
	if order == "" {
		order = "ctid"
		if t.relkind == "p" {
			order = "tableoid, ctid"
		}
	}
	sample := func(cols string) string {
		return fmt.Sprintf("SELECT %s FROM %s ORDER BY %s LIMIT %d", cols, t.from(), order, s.rows)
	}
	loads := []sliceLoad{{table: t, role: "slice", query: sample(t.cols)}}
	idx := map[string]int{}
	var conds [][]string
	for _, r := range rels {
		if r.other.qname == t.qname || r.other.cols == "" {
			continue
		}
		i, ok := idx[r.other.qname]
		if !ok {
			i = len(loads)
			idx[r.other.qname] = i
			loads = append(loads, sliceLoad{table: r.other, role: r.dir})
			conds = append(conds, nil)
		} else if !strings.Contains(loads[i].role, r.dir) {
			loads[i].role = "parent+child"
		}
		var cond string
		if r.dir == "parent" {
			// Los padres que la muestra apunta: como mucho uno por fila.
			cond = fmt.Sprintf("SELECT tableoid, ctid FROM %s WHERE (%s) IN (%s)", r.other.from(), r.ref, sample(r.fk))
		} else {
			// Los hijos de la muestra, con tope.
			cond = fmt.Sprintf("SELECT tableoid, ctid FROM (SELECT tableoid, ctid FROM %s WHERE (%s) IN (%s) LIMIT %d) x",
				r.other.from(), r.fk, sample(r.ref), s.related)
		}
		conds[i-1] = append(conds[i-1], cond)
	}
	for i := 1; i < len(loads); i++ {
		o := loads[i].table
		loads[i].query = fmt.Sprintf("SELECT %s FROM %s WHERE (tableoid, ctid) IN (%s)", o.cols, o.from(), strings.Join(conds[i-1], " UNION ALL "))
	}
	return loads
}

// sliceFillScript es el guion de la máquina de construcción: el esquema por
// una tubería, los cargadores de cada tabla (nombres, no datos) en el tmpfs y
// una sola sesión de psql en producción que saca cada tabla con \copy TO
// PROGRAM hacia su cargador. Ningún dato toca un fichero.
func sliceFillScript(s *cloneSource, loads []sliceLoad) string {
	var b strings.Builder
	b.WriteString("# " + sliceFillMarker + "\nset -eu\nset -o pipefail\n" + cloneMarkerSnippet)
	b.WriteString("C=" + cloneDir + "\nU=" + shq(s.user) + "\nD=" + shq(s.db) + "\n")
	b.WriteString(`rc=0
pg_dump -h ` + cloneDomain + ` -p 5432 -U "$U" -d "$D" --schema-only --no-owner --no-privileges \
    --no-publications --no-subscriptions --no-tablespaces --no-security-labels 2>"$C/dump.err" \
  | su -s /bin/sh postgres -c "psql -X -q -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -h $C -p ` + clonePort + ` -d staging" >/dev/null 2>"$C/restore.err" || rc=$?
if [ "$rc" -ne 0 ]; then
  grep '^pg_dump:' "$C/dump.err" | head -n 20 >&2 || true
  grep -E 'ERROR: +[0-9A-Z]{5}$' "$C/restore.err" | head -n 5 >&2 || true
  exit 1
fi
rm -f "$C"/slice-ok-* "$C/load.err"
cat > "$C/slice-load.sh" <<'KLINGEOF'
set -eu
case "${1:-}" in ''|*[!0-9]*) exit 2 ;; esac
su -s /bin/sh postgres -c "psql -X -q -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -h ` + cloneDir + ` -p ` + clonePort + ` -d staging -f ` + cloneDir + `/slice-load-$1.sql" >/dev/null 2>>` + cloneDir + `/load.err
touch "` + cloneDir + `/slice-ok-$1"
KLINGEOF
`)
	for i, l := range loads {
		fmt.Fprintf(&b, "cat > \"$C/slice-load-%d.sql\" <<'KLINGEOF'\nSET session_replication_role = replica;\n\\copy %s (%s) FROM pstdin\nKLINGEOF\n",
			i, l.table.qname, l.table.cols)
	}
	b.WriteString(`psql -X -q -v ON_ERROR_STOP=1 -v VERBOSITY=sqlstate -h ` + cloneDomain + ` -p 5432 -U "$U" -d "$D" >/dev/null 2>"$C/extract.err" <<'KLINGEOF' || rc=$?
BEGIN ISOLATION LEVEL REPEATABLE READ, READ ONLY;
`)
	for i, l := range loads {
		fmt.Fprintf(&b, "\\copy (%s) TO PROGRAM 'sh %s/slice-load.sh %d'\n", l.query, cloneDir, i)
	}
	b.WriteString("COMMIT;\nKLINGEOF\n")
	b.WriteString(`if [ "$rc" -ne 0 ]; then
  grep -E 'ERROR: +[0-9A-Z]{5}$' "$C/extract.err" | head -n 5 >&2 || true
  grep -E 'ERROR: +[0-9A-Z]{5}$' "$C/load.err" 2>/dev/null | head -n 5 >&2 || true
  echo "extracting the slice from the source failed" >&2
  exit 1
fi
`)
	fmt.Fprintf(&b, "for n in $(seq 0 %d); do\n  [ -f \"$C/slice-ok-$n\" ] || { echo \"loading part $n of the slice failed\" >&2; exit 1; }\ndone\n", len(loads)-1)
	return b.String()
}

// sliceFixupSQL corre en la preparación, como superusuario: claves foráneas
// que las filas cargadas no cumplen a NOT VALID (o fuera), secuencias tras el
// máximo copiado, y cuántas filas quedaron en cada tabla cargada.
func sliceFixupSQL(loads []sliceLoad) string {
	var b strings.Builder
	b.WriteString(`-- ` + sliceFixupMarker + `
CREATE TEMP TABLE kling_slice_fk (k text, name text);
DO $kling$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.conrelid::regclass AS tbl, c.conname, pg_get_constraintdef(c.oid) AS def
      FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
      WHERE c.contype = 'f' AND c.convalidated AND c.conparentid = 0
        AND n.nspname NOT IN ('pg_catalog', 'information_schema')
      ORDER BY c.oid
  LOOP
    BEGIN
      -- Quitarla y ponerla de nuevo la valida contra lo cargado.
      EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.conname);
      EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s', r.tbl, r.conname, r.def);
    EXCEPTION WHEN foreign_key_violation THEN
      EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.conname);
      BEGIN
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s NOT VALID', r.tbl, r.conname, r.def);
        INSERT INTO kling_slice_fk VALUES ('notvalid', format('%s.%I', r.tbl, r.conname));
      EXCEPTION WHEN others THEN
        -- Postgres < 18 no admite NOT VALID en una particionada: queda fuera.
        INSERT INTO kling_slice_fk VALUES ('dropped', format('%s.%I', r.tbl, r.conname));
      END;
    END;
  END LOOP;
END
$kling$;
DO $kling$
DECLARE r record; m bigint;
BEGIN
  FOR r IN SELECT d.objid::regclass AS seq, d.refobjid::regclass AS tbl, a.attname
      FROM pg_depend d JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
      JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
      WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass
        AND d.deptype IN ('a', 'i')
  LOOP
    BEGIN
      EXECUTE format('SELECT max(%I)::bigint FROM %s', r.attname, r.tbl) INTO m;
      IF m IS NOT NULL THEN PERFORM setval(r.seq, m); END IF;
    EXCEPTION WHEN others THEN
      NULL; -- una secuencia que no admite ese valor se queda como está
    END;
  END LOOP;
END
$kling$;
SELECT 'sl|fk|' || k || '|' || name FROM kling_slice_fk ORDER BY name;
`)
	for i, l := range loads {
		fmt.Fprintf(&b, "SELECT 'sl|rows|%d|' || count(*) FROM %s;\n", i, l.table.from())
	}
	return b.String()
}

// parseSliceFixup lee la salida de sliceFixupSQL.
func parseSliceFixup(out []byte, loads []sliceLoad) (*sliceResult, error) {
	res := &sliceResult{Loaded: make([]sliceCount, len(loads)), NotValidFKs: []string{}, DroppedFKs: []string{}}
	seen := make([]bool, len(loads))
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "sl|fk|notvalid|"):
			res.NotValidFKs = append(res.NotValidFKs, dbmaskSafeLong(strings.TrimPrefix(line, "sl|fk|notvalid|")))
		case strings.HasPrefix(line, "sl|fk|dropped|"):
			res.DroppedFKs = append(res.DroppedFKs, dbmaskSafeLong(strings.TrimPrefix(line, "sl|fk|dropped|")))
		case strings.HasPrefix(line, "sl|rows|"):
			f := strings.Split(line, "|")
			if len(f) != 4 {
				return nil, errors.New("unexpected answer from the staging Postgres")
			}
			i, err1 := strconv.Atoi(f[2])
			n, err2 := strconv.ParseInt(f[3], 10, 64)
			if err1 != nil || err2 != nil || i < 0 || i >= len(loads) {
				return nil, errors.New("unexpected answer from the staging Postgres")
			}
			seen[i] = true
			res.Loaded[i] = sliceCount{Table: loads[i].table.plain, Role: loads[i].role, Rows: n}
		}
	}
	for _, ok := range seen {
		if !ok {
			return nil, errors.New("the staging Postgres did not count every sliced table")
		}
	}
	return res, nil
}

// dbmaskSafeLong es dbmaskSafe con sitio para un nombre calificado.
func dbmaskSafeLong(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// ── informe y fichero del slice ─────────────────────────────────────────────

func writeSliceReport(w io.Writer, rep *dbmask.Report, res *sliceResult, asJSON bool) error {
	if asJSON {
		e := json.NewEncoder(w)
		e.SetIndent("", "  ")
		return e.Encode(struct {
			*dbmask.Report
			Slice *sliceResult `json:"slice"`
		}{rep, res})
	}
	if err := rep.WriteText(w); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nslice of %s: up to %d row(s), up to %d related row(s) per foreign key pointing to it\n", res.Table, res.RowsLimit, res.RelatedMax)
	for _, l := range res.Loaded {
		fmt.Fprintf(w, "  %-8s %s: %d row(s)\n", l.Role, l.Table, l.Rows)
	}
	fmt.Fprintln(w, "  every other table: empty (schema only)")
	for _, fk := range res.NotValidFKs {
		fmt.Fprintf(w, "  NOT VALID: %s (it points to rows that were not copied; new rows are still checked)\n", fk)
	}
	for _, fk := range res.DroppedFKs {
		fmt.Fprintf(w, "  DROPPED:   %s (NOT VALID is not possible on it before Postgres 18)\n", fk)
	}
	fmt.Fprintf(w, "observe the statements that touch it: kling db observe <copy>, then kling db observe -report <copy>\n")
	_, err := fmt.Fprintf(w, "warning: %s\n", statsWarning)
	return err
}

// sliceMeta es lo que queda en el host de un golden hecho con slice.
type sliceMeta struct {
	Table   string    `json:"table"`
	Source  string    `json:"source"` // usuario@host:puerto/base, sin contraseña
	Rows    int       `json:"rows"`
	Related int       `json:"related_rows"`
	Created time.Time `json:"created"`
}

// writeSliceMeta deja slice.json (0600) junto a la contraseña del golden.
func writeSliceMeta(golden, source string, res *sliceResult) error {
	if res == nil || !goldenNamePattern.MatchString(golden) {
		return errors.New("no slice")
	}
	d, err := dbstate.Dir()
	if err != nil {
		return err
	}
	dir := filepath.Join(d, golden)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(sliceMeta{Table: res.Table, Source: source, Rows: res.RowsLimit, Related: res.RelatedMax, Created: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, sliceFile+".new")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, sliceFile))
}

// readSliceMeta lee slice.json de un golden; nil si no lo hay.
func readSliceMeta(golden string) (*sliceMeta, error) {
	if !goldenNamePattern.MatchString(golden) {
		return nil, nil
	}
	d, err := dbstate.Dir()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(d, golden, sliceFile)
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	b, err := readLimited(p, 64<<10)
	if err != nil {
		return nil, err
	}
	var m sliceMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &m, nil
}
