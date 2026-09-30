package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTranslationHealth(t *testing.T) {
	if got := splitABIs(" x86_64, arm64-v8a ,"); !reflect.DeepEqual(got, []string{"x86_64", "arm64-v8a"}) {
		t.Fatalf("splitABIs %q", got)
	}
	if splitABIs("") != nil || bridgeName("0") != "" || bridgeName("libndk_translation.so") != "libndk_translation.so" {
		t.Fatal("empty values")
	}
	d := t.TempDir()
	img := filepath.Join(d, "IMAGE.txt")
	os.WriteFile(img, []byte("redroid=x\narm_translation=libndk\nnative_bridge=libndk_translation.so\n"), 0o644)
	if v := imageValue(img, "arm_translation"); v != "libndk" {
		t.Fatalf("arm_translation %q", v)
	}
	if v := imageValue(img, "nope"); v != "" || imageValue(filepath.Join(d, "none"), "arm_translation") != "" {
		t.Fatal("missing key")
	}
	bf := filepath.Join(d, "arm64_exe")
	os.WriteFile(bf, []byte("enabled\ninterpreter /system/bin/kindling-ndk-binfmt\nflags: P\n"), 0o644)
	if binfmtState(bf) != "enabled" || binfmtState(filepath.Join(d, "x")) != "" {
		t.Fatal("binfmtState")
	}
}
