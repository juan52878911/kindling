package oci

import (
	"encoding/json"
	"strings"
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

// Sin la plataforma del daemon, el error lista las que hay y qué hacer.
func TestErrSinPlataforma(t *testing.T) {
	var ds []Descriptor
	_ = json.Unmarshal([]byte(`[
		{"digest":"att","platform":{"os":"unknown","architecture":"unknown"}},
		{"digest":"amd","platform":{"os":"linux","architecture":"amd64"}},
		{"digest":"v7","platform":{"os":"linux","architecture":"arm","variant":"v7"}},
		{"digest":"amd2","platform":{"os":"linux","architecture":"amd64"}}]`), &ds)
	err := errSinPlataforma("x/solo-amd", "sha256:abc", "arm64", ds)
	want := "x/solo-amd is published only for linux/amd64, linux/arm/v7, not linux/arm64 (the daemon's architecture): " +
		"look for a tag published for linux/arm64, or build one (docker buildx build --platform linux/arm64); " +
		"-arch amd64 imports it for amd64 hosts (it does not boot on this one)"
	if err == nil || err.Error() != want {
		t.Fatalf("got  %v\nwant %s", err, want)
	}
	if err := errSinPlataforma("x", "d", "arm64", nil); err == nil || !strings.Contains(err.Error(), "lists no platforms") {
		t.Fatalf("vacío: %v", err)
	}
}
