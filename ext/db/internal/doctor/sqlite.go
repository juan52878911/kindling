package doctor

// Revisión de una copia SQLite de kling db (etiqueta kling.db.engine=sqlite):
// no hay servidor, red ni contraseña, así que lo básico es el fichero: que
// exista, que esté sano (PRAGMA quick_check) y que solo lo lea su dueño.
// Reglas SQnnn.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
)

// sqlitePath es el fichero de la base db dentro de la copia (el de kling db).
func sqlitePath(db string) string { return "/var/lib/kling-db/" + db + ".sqlite" }

// sqliteOpen abre la base en solo lectura sin crearla si no existe; lo
// variable (el nombre) ya pasó reDB.
func sqliteOpen(db string) string {
	return `f=` + sqlitePath(db) + ` && [ -f "$f" ] && exec sqlite3 -bail -readonly "$f"`
}

// sqliteDB es la base de la copia: la etiqueta kling.db.database, o DBNAME
// del conn.env del dorado, o "appdb".
func sqliteDB(state, golden string, labels map[string]string) (string, error) {
	db := "appdb"
	if golden != "" && reGolden.MatchString(golden) {
		if b, err := readSmall(filepath.Join(state, golden, "conn.env")); err == nil {
			sc := bufio.NewScanner(bytes.NewReader(b))
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "DBNAME="); ok {
					db = strings.TrimSpace(v)
				}
			}
		}
	}
	if v := labels["kling.db.database"]; v != "" {
		db = v
	}
	if !reDB.MatchString(db) {
		return "", fmt.Errorf("doctor: database %q is not a plain identifier", safe(db, 64))
	}
	return db, nil
}

// sqliteCopy revisa una copia SQLite. mc ya se leyó y está corriendo.
func sqliteCopy(ctx context.Context, k klingc.Kling, mc *api.Machine, state string, r *report) error {
	golden := mc.Labels[LabelGolden]
	db, err := sqliteDB(state, golden, mc.Labels)
	if err != nil {
		return err
	}
	p := sqlitePath(db)
	out, err := k.Run(ctx, strings.NewReader("PRAGMA quick_check;\n"), "exec", "-i", "-timeout", "5m", mc.ID, "--", "sh", "-c", sqliteOpen(db))
	if err != nil {
		return fmt.Errorf("doctor: cannot open %s in %s: %w", p, safe(mc.Name, 64), err)
	}
	if res := strings.TrimSpace(string(out)); res != "ok" {
		first, _, _ := strings.Cut(res, "\n")
		r.add("SQ001", High, "restore the copy (kling db reset) or rebuild the golden",
			"PRAGMA quick_check on %s reports: %s", p, safe(first, 160))
	}
	// El modo del fichero: stat de busybox y de coreutils entienden -c %a.
	if st, err := k.Run(ctx, nil, "exec", mc.ID, "--", "stat", "-c", "%a", p); err != nil {
		checkFailed(r, "SQ002", "the file mode", err)
	} else if m, perr := strconv.ParseUint(strings.TrimSpace(string(st)), 8, 32); perr != nil {
		checkFailed(r, "SQ002", "the file mode", fmt.Errorf("unexpected stat output"))
	} else if m&0o007 != 0 {
		r.add("SQ002", Warn, "chmod 600 "+p+" (or 660 for a group of the guest)",
			"%s is open to every user of the guest (mode %04o)", p, m)
	}
	r.add("SQ050", Info, "", "sqlite copy: no network server and no password; access is kling exec / kling shell on the machine")

	if golden == "" {
		r.add("DB059", Info, "", "not a kling db copy (no %s label): copy checks skipped", LabelGolden)
		return nil
	}
	if !reGolden.MatchString(golden) {
		r.add("DB059", High, "recreate the copy from a golden with a plain name",
			"label %s has an unexpected value %s: copy checks skipped", LabelGolden, q(golden))
		return nil
	}
	if mc.Labels[LabelState] != StateReady {
		r.add("SQ053", High, "finish preparing the copy or destroy it; kling db connect refuses it until then",
			"%s is %s, not %q", LabelState, q(mc.Labels[LabelState]), StateReady)
	}
	return nil
}
