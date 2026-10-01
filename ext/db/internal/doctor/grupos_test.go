package doctor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// 138 hallazgos DB010 (69 políticas, USING y WITH CHECK) salen en una línea
// con el recuento y los objetos sin repetir; con -all, uno a uno.
func TestInformeAgrupaLoRepetido(t *testing.T) {
	r := &report{target: "x"}
	for i := range 69 {
		obj := fmt.Sprintf("svc.t%02d", i)
		for _, k := range []string{"USING", "WITH CHECK"} {
			r.addObj("DB010", Critical, FailOpenFix, "", obj, "policy p on %s is fail-open in %s", obj, k)
		}
	}
	r.add("DB001", Critical, "fix it", "login role is SUPERUSER")
	r.addObj("DB011", High, "ALTER TABLE a ENABLE...", "ALTER TABLE <table> ENABLE ROW LEVEL SECURITY on each one", "s.a", "table a has tenant_id")
	var out bytes.Buffer
	r.write(&out)
	s := out.String()
	t.Log("\n" + s)
	if n := strings.Count(s, "DB010"); n != 1 {
		t.Errorf("DB010 aparece %d veces", n)
	}
	if !strings.Contains(s, "138 findings on 69 objects") || !strings.Contains(s, "(+61 more)") {
		t.Error("sin recuento u objetos")
	}
	// Uno solo no se agrupa; el resumen sigue contando todos.
	if !strings.Contains(s, "table a has tenant_id") || !strings.Contains(s, "summary: 139 critical, 1 high") {
		t.Error("resumen o DB011 suelto mal")
	}
	if n := strings.Count(s, "\n"); n > 12 {
		t.Errorf("%d líneas", n)
	}
	r.all = true
	out.Reset()
	r.write(&out)
	if n := strings.Count(out.String(), "DB010"); n != 138 {
		t.Errorf("-all: DB010 aparece %d veces", n)
	}
}

// El rol de la aplicación sale de la etiqueta de la copia aunque el golden
// no tenga conn.env (uno guardado con kling save): sin DB052/DB020 falsos.
func TestAppRoleDeLaEtiqueta(t *testing.T) {
	d := t.TempDir()
	role, err := appRoleOf(d, "aura-main", map[string]string{"kling.db.role": "crm_user"})
	if err != nil || role != "crm_user" {
		t.Fatalf("%q %v", role, err)
	}
	if role, _ := appRoleOf(d, "aura-main", nil); role != "app" {
		t.Errorf("sin etiqueta ni conn.env: %q", role)
	}
	if _, err := appRoleOf(d, "g", map[string]string{"kling.db.role": "x; drop"}); err == nil {
		t.Error("aceptó un rol raro")
	}
}
