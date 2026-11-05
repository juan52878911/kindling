package api

import (
	"testing"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// -share SRC:DST:ro|rw:virtiofs pide virtio-fs; :fuse, FUSE; sin nada, lo del
// daemon. Solo detrás de un modo vivo.
func TestParseShareTransporte(t *testing.T) {
	buenos := map[string]ShareSpec{
		"/src:/app:rw:virtiofs": {Mode: share.ModeRW, Mount: "/app", Source: "/src", Transport: share.TransportVirtioFS},
		"/src:/app:ro:fuse":     {Mode: share.ModeRO, Mount: "/app", Source: "/src", Transport: share.TransportFUSE},
		"/src:/app:rw":          {Mode: share.ModeRW, Mount: "/app", Source: "/src"},
		"./a:b:/app:ro":         {Mode: share.ModeRO, Mount: "/app", Source: "./a:b"},
		"/src:/app":             {Mode: share.ModeCopy, Mount: "/app", Source: "/src"},
	}
	for v, want := range buenos {
		got, err := ParseShare(v)
		if err != nil || got != want {
			t.Errorf("ParseShare(%q) = %+v, %v; want %+v", v, got, err, want)
		}
	}
	for _, v := range []string{"/src:/app:virtiofs", "/src:/app:copy:virtiofs", "/src:/app:rw:virtiofs:fuse"} {
		if s, err := ParseShare(v); err == nil {
			t.Errorf("ParseShare(%q) = %+v: accepted", v, s)
		}
	}
}
