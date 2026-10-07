package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// La sonda de una imagen sin sh: el argv en JSON detrás del #!.
func TestReadArgvFile(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{
		"#!/usr/local/bin/kling-guest -exec-json\n[\"/healthcheck\",\"--port\",\"8080\"]\n": "/healthcheck --port 8080",
		"#!/usr/local/bin/kling-guest -exec-json\n[\"wget\",\"-q\",\"it's\"]":               "wget -q it's",
		"#!/usr/local/bin/kling-guest -exec-json\n[]\n":                                     "",
		"#!/usr/local/bin/kling-guest -exec-json\nwget -q\n":                                "",
		"#!/usr/local/bin/kling-guest -exec-json":                                           "",
	} {
		p := filepath.Join(dir, "ready")
		os.WriteFile(p, []byte(body), 0o755)
		argv, err := readArgvFile(p)
		if got := strings.Join(argv, " "); got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q, %v; want %q", body, got, err, want)
		}
	}
}
