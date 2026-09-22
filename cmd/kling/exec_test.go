package main

import "testing"

func TestSplitRemote(t *testing.T) {
	for in, want := range map[string]struct {
		ref, path string
		remote    bool
	}{
		"sb1:/tmp/x":   {"sb1", "/tmp/x", true},
		"sb1:rel":      {"sb1", "rel", true},
		"./a:b":        {"", "./a:b", false},
		"/abs/a:b":     {"", "/abs/a:b", false},
		"-":            {"", "-", false},
		"local.txt":    {"", "local.txt", false},
		":sin-maquina": {"", ":sin-maquina", false},
	} {
		ref, path, remote := splitRemote(in)
		if ref != want.ref || path != want.path || remote != want.remote {
			t.Errorf("%q: %q %q %v", in, ref, path, remote)
		}
	}
	if baseName("/a/b/c.txt") != "c.txt" || baseName("c") != "c" || baseName("/a/b/") != "b" {
		t.Error("baseName")
	}
}
