package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Un snapshot con carpetas por virtio-fs es de la versión 4 (uno anterior las
// perdería al restaurar) y lleva el estado de su servidor FUSE; sin ellas
// sigue siendo de la más baja que lo describe.
func TestSnapshotVersion4VirtioFS(t *testing.T) {
	s := sample()
	s.Vsock = true
	s.VirtioFS = []VirtioFS{{Tag: 0, ReadOnly: true, State: []byte{1, 2, 3}}, {Tag: 5}}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := s.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"kling_vz": 4`) {
		t.Fatalf("want kling_vz 4 in\n%s", b)
	}
	got, err := Decode(strings.NewReader(string(b)))
	if err != nil || len(got.VirtioFS) != 2 || !got.VirtioFS[0].ReadOnly || string(got.VirtioFS[0].State) != "\x01\x02\x03" || got.VirtioFS[1].Tag != 5 {
		t.Fatalf("releído %+v, %v", got, err)
	}
}

func TestValidateVirtioFS(t *testing.T) {
	ok := []VirtioFS{{Tag: 0}, {Tag: 7}}
	if err := ValidateVirtioFS(ok); err != nil {
		t.Fatal(err)
	}
	for nombre, v := range map[string][]VirtioFS{
		"repetido":  {{Tag: 1}, {Tag: 1}},
		"negativo":  {{Tag: -1}},
		"demasiado": {{Tag: 8}},
		"nueve":     {{Tag: 0}, {Tag: 1}, {Tag: 2}, {Tag: 3}, {Tag: 4}, {Tag: 5}, {Tag: 6}, {Tag: 7}, {Tag: 0}},
	} {
		if err := ValidateVirtioFS(v); err == nil {
			t.Errorf("%s: accepted", nombre)
		}
	}
	s := sample()
	s.VirtioFS = []VirtioFS{{Tag: 2}, {Tag: 2}}
	if err := s.Validate(); err == nil {
		t.Error("Validate accepted a repeated virtio-fs tag")
	}
}
