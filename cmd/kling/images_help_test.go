package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Cada subcomando que despacha cmdImages sale en `kling help image` con su
// bloque; `kling help image import` daba "unknown command".
func TestImageSubcommandsHaveHelp(t *testing.T) {
	src, err := os.ReadFile("images.go")
	if err != nil {
		t.Fatal(err)
	}
	// Solo el primer nombre de cada case: los demás son sinónimos.
	caseRe := regexp.MustCompile(`(?m)^\tcase "([a-z-]+)"`)
	img := coreCommand("image")
	if img == nil {
		t.Fatal("image missing from the tree")
	}
	for _, m := range caseRe.FindAllStringSubmatch(string(src), -1) {
		sub := m[1]
		if sub == "refresh" { // solo redirige a kling mcp refresh-bridge
			continue
		}
		if !slices.Contains(img.Subcommands, sub) {
			t.Errorf("image %s: dispatched but not in Subcommands", sub)
		}
		if subBlock(img, sub) == "" {
			t.Errorf("image %s: no usage block", sub)
		}
	}
	b := subBlock(img, "import")
	for _, f := range []string{"-name", "-replace", "-json", "-e ", "-env-file", "-user", "-entrypoint",
		"-restart", "-max-size", "-arch", "-- cmd", "-archive", "-image R"} {
		if !strings.Contains(b, f) {
			t.Errorf("image import help lacks %s:\n%s", f, b)
		}
	}
}
