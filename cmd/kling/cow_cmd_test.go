package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestParseCrecimiento(t *testing.T) {
	casos := []struct {
		in   string
		want api.GrowCoWStoreRequest
		mal  bool
	}{
		{"32G", api.GrowCoWStoreRequest{SizeMiB: 32 << 10}, false},
		{"+8G", api.GrowCoWStoreRequest{AddMiB: 8 << 10}, false},
		{"+512M", api.GrowCoWStoreRequest{AddMiB: 512}, false},
		{"20480", api.GrowCoWStoreRequest{SizeMiB: 20480}, false},
		{"+", api.GrowCoWStoreRequest{}, true},
		{"-8G", api.GrowCoWStoreRequest{}, true},
		{"mucho", api.GrowCoWStoreRequest{}, true},
	}
	for _, c := range casos {
		got, err := parseCrecimiento(c.in)
		if (err != nil) != c.mal || got != c.want {
			t.Errorf("%q: %+v %v", c.in, got, err)
		}
	}
}

// #61: pasado el 85 % el almacén avisa en kling info y en kling doctor, con
// el comando para agrandarlo.
func TestCoWCasiLleno(t *testing.T) {
	lleno := &api.CoWStore{Path: "/r/cow", FS: "xfs", Mounted: true, SizeMiB: 16384, FreeMiB: 1000}
	holgado := &api.CoWStore{Path: "/r/cow", FS: "xfs", Mounted: true, SizeMiB: 16384, FreeMiB: 8000}
	if casiLlenoCoW(holgado) != "" || casiLlenoCoW(nil) != "" || casiLlenoCoW(&api.CoWStore{Mounted: true}) != "" {
		t.Error("avisa sin estar lleno")
	}
	if c := casiLlenoCoW(lleno); c != "93% used" {
		t.Errorf("lleno: %q", c)
	}
	l := lineaCoW(&api.CoWInfo{Setting: "auto", Mode: "store", Store: lleno})
	if !strings.Contains(l, "93% used: kling cow grow") {
		t.Errorf("info: %q", l)
	}
	d, _ := checkCoW(&api.CoWInfo{Setting: "auto", Mode: "store", Store: lleno})
	if d.State != doctorWarn || !strings.Contains(d.Detail, "93% used") || !strings.Contains(d.Fix, "kling cow grow") {
		t.Errorf("doctor: %+v", d)
	}
	d, _ = checkCoW(&api.CoWInfo{Setting: "auto", Mode: "store", Store: holgado})
	if d.State != doctorOK {
		t.Errorf("doctor con sitio: %+v", d)
	}
}
