package doctor

// Revisión de una copia Garage (S3) de kling db (etiqueta
// kling.db.engine=garage): por la CLI de Garage dentro del invitado. Reglas
// GAnnn. Lo que devuelve el servidor es dato no fiable: se escapa antes de
// imprimirlo.

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/klingc"
	"github.com/juan52878911/kindling/pkg/api"
)

var reGarageKey = regexp.MustCompile(`\bGK[0-9a-f]{24}\b`)

func garageCopy(ctx context.Context, k klingc.Kling, mc *api.Machine, state string, r *report) error {
	golden := mc.Labels[LabelGolden]
	out, err := k.Run(ctx, nil, "exec", "-timeout", "60s", mc.ID, "--", "/garage", "-c", "/etc/garage.toml", "key", "list")
	if err != nil {
		return fmt.Errorf("doctor: cannot query Garage in %s: %w", safe(mc.Name, 64), err)
	}
	keys := reGarageKey.FindAllString(string(out), -1)
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
		r.add("GA053", High, "finish preparing the copy (key rotation) or destroy it; kling db connect refuses it until then",
			"%s is %s, not %q", LabelState, q(mc.Labels[LabelState]), StateReady)
	}
	if len(keys) != 1 {
		r.add("GA052", High, "rotate the key with kling db rotate (it leaves exactly one)",
			"the copy holds %d access key(s); kling db gives it exactly one", len(keys))
		return nil
	}
	if gp, err := readSmall(filepath.Join(state, golden, "password")); err == nil {
		if dbstate.S3AccessKey(strings.TrimRight(string(gp), "\r\n")) == keys[0] {
			r.add("GA052", Critical, "destroy this copy; kling db must rotate the key before marking a copy ready",
				"the access key of this copy is still the golden's: whoever knows %s's secret can use this copy", q(golden))
		}
	} else {
		r.add("GA052", Warn, fmt.Sprintf("keep the golden's secret in %s so the rotation can be checked", filepath.Join(state, golden, "password")),
			"cannot verify that the key was rotated away from the golden's")
	}
	pw, err := dbstate.ReadPassword(mc.ID)
	if err != nil {
		r.add("GA054", High, "destroy the copy and create it again: the rotated secret is only known to the host that rotated it",
			"no usable host secret for this copy: kling db connect refuses it")
		return nil
	}
	if dbstate.S3AccessKey(pw) != keys[0] {
		r.add("GA054", High, "rotate the key again (kling db rotate) or recreate the copy",
			"the host secret does not match the copy's access key: kling db connect will fail")
	}
	return nil
}
