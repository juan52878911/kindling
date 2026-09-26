package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestWriteSnapshots(t *testing.T) {
	var b bytes.Buffer
	if err := writeSnapshots(&b, nil, true); err != nil || strings.TrimSpace(b.String()) != "[]" {
		t.Fatalf("una lista vacía en JSON es []: %q", b.String())
	}
	b.Reset()
	list := []*api.Snapshot{{Name: "tpl", Image: "toolchain", VCPUs: 1, MemMiB: 256, MemBytes: 40 << 20, CreatedAt: time.Now()}}
	if err := writeSnapshots(&b, list, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "NAME") || !strings.Contains(b.String(), "tpl") || !strings.Contains(b.String(), "40M") {
		t.Fatalf("%s", b.String())
	}
}

func TestWriteSnapshot(t *testing.T) {
	s := &api.Snapshot{Name: "tpl", Image: "toolchain", VCPUs: 2, MemMiB: 512, MemMaxMiB: 1024,
		CreatedAt: time.Now().Add(-time.Hour), Instances: 3,
		Volumes:     []api.VolumeAttachment{{Name: "data", Mount: "/data", ReadOnly: true}},
		Annotations: map[string]json.RawMessage{"mcp.catalog": json.RawMessage(`{"tools": ["a", "b"]}`), "b": json.RawMessage(`1`)}}
	var b bytes.Buffer
	if err := writeSnapshot(&b, s, false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"name:        tpl", "2 / 512 MiB (ceiling 1024 MiB)", "instances:   3",
		"data -> /data (ro)", "annotations:", `mcp.catalog  {"tools": ["a", "b"]}`, "kling run -from tpl"} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en:\n%s", want, out)
		}
	}
	if strings.Index(out, "  b  1") > strings.Index(out, "mcp.catalog") {
		t.Error("las anotaciones van ordenadas")
	}
	b.Reset()
	if err := writeSnapshot(&b, s, true); err != nil {
		t.Fatal(err)
	}
	var back api.Snapshot
	if err := json.Unmarshal(b.Bytes(), &back); err != nil || back.Name != "tpl" || len(back.Annotations) != 2 {
		t.Fatalf("%v %+v", err, back)
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt(json.RawMessage("{\n  \"a\":   1\n}"), 60); got != `{ "a": 1 }` {
		t.Fatalf("%q", got)
	}
	long := json.RawMessage(`"` + strings.Repeat("ñ", 100) + `"`)
	got := excerpt(long, 10)
	if len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("recorta por runas: %q", got)
	}
}

func TestSnapshotsInspectContraUnDaemonFalso(t *testing.T) {
	d := fakeSnapshotsMux()
	c := fakeDaemon(t, d)
	s, err := c.Snapshot(t.Context(), "tpl")
	if err != nil || s.Name != "tpl" {
		t.Fatalf("%v %+v", err, s)
	}
	_, err = c.Snapshot(t.Context(), "nope")
	if err == nil || hintFor(err) != "kling snapshots ls" {
		t.Fatalf("%v / %q", err, hintFor(err))
	}
}
