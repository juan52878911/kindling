package oci

import (
	"encoding/json"
	"testing"
)

func TestPickPlatform(t *testing.T) {
	var ds []Descriptor
	_ = json.Unmarshal([]byte(`[
		{"digest":"att","platform":{"os":"unknown","architecture":"unknown"}},
		{"digest":"v3","platform":{"os":"linux","architecture":"amd64","variant":"v3"}},
		{"digest":"amd","platform":{"os":"linux","architecture":"amd64"}},
		{"digest":"v7","platform":{"os":"linux","architecture":"arm","variant":"v7"}},
		{"digest":"v8","platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]`), &ds)
	for arch, want := range map[string]string{"amd64": "amd", "arm64": "v8", "arm": "v7", "s390x": ""} {
		got := ""
		if p := pickPlatform(ds, arch); p != nil {
			got = p.Digest
		}
		if got != want {
			t.Errorf("%s: picked %q, want %q", arch, got, want)
		}
	}
}
