package gateway

import "testing"

func TestCoerce(t *testing.T) {
	cases := []struct{ name, schema, in, want string }{
		{"array como objeto indexado",
			`{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}}}`,
			`{"paths":{"0":"/a","1":"/b"}}`, `{"paths":["/a","/b"]}`},
		{"array como string JSON",
			`{"type":"object","properties":{"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"}}}}}}`,
			`{"edits":"[{\"oldText\":\"x\"}]"}`, `{"edits":[{"oldText":"x"}]}`},
		{"número como string",
			`{"type":"object","properties":{"head":{"type":"integer"}}}`,
			`{"head":"5"}`, `{"head":5}`},
		{"booleano como string",
			`{"type":"object","properties":{"dryRun":{"type":"boolean"}}}`,
			`{"dryRun":"false"}`, `{"dryRun":false}`},
		{"lo correcto se deja intacto",
			`{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}}}`,
			`{"paths":["/a"]}`, `{"paths":["/a"]}`},
		{"objeto real NO se convierte en array",
			`{"type":"object","properties":{"meta":{"type":"array"}}}`,
			`{"meta":{"a":1}}`, `{"meta":{"a":1}}`},
	}
	for _, c := range cases {
		got := string(coerceArgs([]byte(c.in), []byte(c.schema)))
		if got != c.want {
			t.Errorf("%s:\n  esperado %s\n  obtenido %s", c.name, c.want, got)
		}
	}
}
