package api

import (
	"strings"
	"testing"
)

func TestParseUlimit(t *testing.T) {
	for in, want := range map[string]Ulimit{
		"nofile=65536":       {"nofile", 65536, 65536},
		"nofile=1024:65536":  {"nofile", 1024, 65536},
		"memlock=unlimited":  {"memlock", UlimitUnlimited, UlimitUnlimited},
		"memlock=-1":         {"memlock", UlimitUnlimited, UlimitUnlimited},
		"nproc=1024:4096":    {"nproc", 1024, 4096},
		"core=0":             {"core", 0, 0},
		"nofile=1:unlimited": {"nofile", 1, UlimitUnlimited},
	} {
		got, err := ParseUlimit(in)
		if err != nil || got != want {
			t.Errorf("ParseUlimit(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "nofile", "nofile=", "files=10", "nofile=abc", "nofile=10:x",
		"nofile=2:1", "nofile=-5", "nofile=unlimited:1", "NOFILE=1", "nofile=1:2:3"} {
		if u, err := ParseUlimit(bad); err == nil {
			t.Errorf("ParseUlimit(%q) accepted: %+v", bad, u)
		}
	}
}

func TestParseUlimitsRejectsDuplicatesAndExcess(t *testing.T) {
	if _, err := ParseUlimits([]string{"nofile=1", "nofile=2"}); err == nil {
		t.Error("duplicate resource accepted")
	}
	var many []string
	for i := 0; i <= MaxUlimits; i++ {
		many = append(many, "nofile=1")
	}
	if _, err := ParseUlimits(many); err == nil {
		t.Error("too many ulimits accepted")
	}
	if us, err := ParseUlimits(nil); err != nil || len(us) != 0 {
		t.Errorf("nil: %v %v", us, err)
	}
}

func TestCheckSysctl(t *testing.T) {
	for k, v := range map[string]string{
		"vm.max_map_count":             "262144",
		"vm.overcommit_memory":         "1",
		"net.core.somaxconn":           "4096",
		"net.ipv4.tcp_max_syn_backlog": "8192",
		"net.ipv4.ip_local_port_range": "1024 65000",
		"kernel.sem":                   "250 32000 100 128",
		"fs.aio-max-nr":                "1048576",
	} {
		if err := CheckSysctl(k, v); err != nil {
			t.Errorf("CheckSysctl(%q, %q): %v", k, v, err)
		}
	}
	for _, c := range [][2]string{
		{"kernel.core_pattern", "|/bin/sh"},   // ejecuta algo como root
		{"kernel.modprobe", "/tmp/x"},         // idem
		{"fs.binfmt_misc.register", "x"},      // idem
		{"kernel.hostname", "x"},              // fuera de la lista
		{"net.ipv4.ip_forward", "1"},          // cambia el papel de la máquina
		{"vm.max_map_count", "1\nkernel.x=2"}, // valor con salto de línea
		{"vm.max_map_count", ""},              // vacío
		{"vm.max_map_count", "$(reboot)"},     // metacaracteres
		{"../etc/passwd", "1"},                // no es una clave
		{"vm/max_map_count", "1"},             // la forma con barras no
		{"net.ipv4.tcp_../../x", "1"},         // prefijo permitido con ruta
		{"", "1"},
	} {
		if err := CheckSysctl(c[0], c[1]); err == nil {
			t.Errorf("CheckSysctl(%q, %q) accepted", c[0], c[1])
		}
	}
	if err := CheckSysctls(map[string]string{"vm.max_map_count": "1", "kernel.core_pattern": "x"}); err == nil ||
		!strings.Contains(err.Error(), "core_pattern") {
		t.Errorf("CheckSysctls: %v", err)
	}
}

func TestParseSysctlFlag(t *testing.T) {
	k, v, err := ParseSysctlFlag("vm.max_map_count=262144")
	if err != nil || k != "vm.max_map_count" || v != "262144" {
		t.Fatalf("%q %q %v", k, v, err)
	}
	for _, bad := range []string{"vm.max_map_count", "kernel.core_pattern=x", "=1"} {
		if _, _, err := ParseSysctlFlag(bad); err == nil {
			t.Errorf("ParseSysctlFlag(%q) accepted", bad)
		}
	}
}
