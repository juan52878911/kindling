package units

import (
	"flag"
	"testing"
	"time"
)

func TestParseMiB(t *testing.T) {
	for in, want := range map[string]int{"512": 512, "512M": 512, "2G": 2048, "1024MiB": 1024, "0.5G": 512, "2g": 2048, "1T": 1 << 20, "2048K": 2} {
		got, err := ParseMiB(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "abc", "2X", "1.5.5G"} {
		if _, err := ParseMiB(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"600": 10 * time.Minute, "10m": 10 * time.Minute, "1h30m": 90 * time.Minute, "0": 0} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("%q: got %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := ParseDuration("-5m"); err == nil {
		t.Error("negative duration accepted")
	}
}

func TestFlags(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	mem := MiBVar(fs, "mem", 256, "")
	ttl := SecondsVar(fs, "ttl", 0, "")
	idle := DurationVar(fs, "idle", time.Minute, "")
	if err := fs.Parse([]string{"-mem", "2G", "-ttl", "10m", "-idle", "90"}); err != nil {
		t.Fatal(err)
	}
	if *mem != 2048 || *ttl != 600 || *idle != 90*time.Second {
		t.Fatalf("mem=%d ttl=%d idle=%s", *mem, *ttl, *idle)
	}
	fs = flag.NewFlagSet("y", flag.ContinueOnError)
	mem = MiBVar(fs, "mem", 256, "")
	if err := fs.Parse(nil); err != nil || *mem != 256 {
		t.Fatalf("default lost: %d %v", *mem, err)
	}
}
