package main

import "testing"

// El informe de tenant-check enseña la forma de la política, no sus valores.
func TestSinLiterales(t *testing.T) {
	casos := map[string]string{
		`(tenant_id)::text = 'acme'`:                           `(tenant_id)::text = '…'`,
		`current_setting('app.tenant_id'::text, true) IS NULL`: `current_setting('app.tenant_id'::text, true) IS NULL`,
		`name = 'o''brien' OR x = 'yz'`:                        `name = '…' OR x = '…'`,
		// Vacío o de un carácter: se ve (el fail-open de AuraCRM es "= ''").
		`current_setting('app.tenant_id'::text, true) = ''::text`: `current_setting('app.tenant_id'::text, true) = ''::text`,
		`estado = 'A'`: `estado = 'A'`,
		`tenant_id = current_setting('app.t')::uuid`: `tenant_id = current_setting('app.t')::uuid`,
		`true`: `true`,
	}
	for in, want := range casos {
		if got := sinLiterales(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
