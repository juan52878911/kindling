package guest

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

const tablaPrueba = "# dm-verity de la capa (constructor debian de kindling). Lo lee /sbin/overlay-init.\n" +
	"0 8192 verity 1 @DEV@ @DEV@ 4096 4096 1024 1025 sha256 aa bb 2 ignore_zero_blocks use_fec_from_device @DEV@\n"

// Con la tabla en la base, la capa se monta por /dev/mapper, después de
// ponerlo; con @DEV@ cambiado por la capa, como el sed del script.
func TestInitVerityPlan(t *testing.T) {
	f := newFakeInit()
	f.files["/proc/cmdline"] = "kling.layer=/dev/vdc"
	f.files[VerityConfPath] = tablaPrueba
	if err := runInit(f, nil); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(f.calls, "\n")
	want := "mkdir /overlay/svc\n" +
		"dmcreate kindling-layer 0 8192 verity 1 /dev/vdc /dev/vdc 4096 4096 1024 1025 sha256 aa bb 2 ignore_zero_blocks use_fec_from_device /dev/vdc\n" +
		"mount /dev/mapper/kindling-layer /overlay/svc ext4 0x1\n"
	if !strings.Contains(plan, want) || strings.Contains(plan, "mount /dev/vdc ") {
		t.Fatalf("plan:\n%s", plan)
	}

	// Sin capa, la tabla no se mira (como el bloque del script, dentro del if).
	g := newFakeInit()
	g.files[VerityConfPath] = tablaPrueba
	if err := runInit(g, nil); err != nil || strings.Contains(strings.Join(g.calls, "\n"), "dmcreate") {
		t.Fatalf("without a layer: %v\n%s", err, strings.Join(g.calls, "\n"))
	}
}

// Falla cerrado: con la tabla y sin verity, ni la capa se monta ni el
// agente arranca.
func TestInitVerityFallaCerrado(t *testing.T) {
	for name, prep := range map[string]func(f *fakeInit){
		"ioctl": func(f *fakeInit) {
			f.files[VerityConfPath] = tablaPrueba
			f.fail["dmcreate kindling-layer 0 8192 verity 1 /dev/vdc /dev/vdc 4096 4096 1024 1025 sha256 aa bb 2 ignore_zero_blocks use_fec_from_device /dev/vdc"] = errors.New("boom")
		},
		"empty table":    func(f *fakeInit) { f.files[VerityConfPath] = "# nada\n\n" },
		"bad table":      func(f *fakeInit) { f.files[VerityConfPath] = "0 x verity 1\n" },
		"unreadable":     func(f *fakeInit) { f.files[VerityConfPath] = ""; f.rerr[VerityConfPath] = errors.New("EIO") },
		"type too long":  func(f *fakeInit) { f.files[VerityConfPath] = "0 8 verityverityverity 1\n" },
		"zero length":    func(f *fakeInit) { f.files[VerityConfPath] = "0 0 verity 1\n" },
		"missing params": func(f *fakeInit) { f.files[VerityConfPath] = "0 8 verity\n" },
	} {
		f := newFakeInit()
		f.files["/proc/cmdline"] = "kling.layer=/dev/vdc"
		prep(f)
		err := runInit(f, nil)
		if err == nil || !strings.Contains(err.Error(), "refusing to mount the layer unverified") {
			t.Fatalf("%s: %v", name, err)
		}
		for _, c := range f.calls {
			if strings.HasPrefix(c, "mount /dev/vdc") || strings.HasPrefix(c, "mount /dev/mapper") || strings.HasPrefix(c, "exec") {
				t.Fatalf("%s: %q after a verity failure", name, c)
			}
		}
	}
}

// El /entrypoint de la capa en una base: el generado sin servicio se hace
// sin sh; cualquier otro se ejecuta; en una imagen de Docker no se mira.
func TestInitEntrypoint(t *testing.T) {
	gen := "#!/bin/sh\n# Generado por el constructor debian de kindling: el agente de invitado es PID 1.\n" +
		"export PATH=" + initPATH + "\nexport HOME=/root\n"
	conEnv := gen + ". /etc/kling/env\nexec /usr/local/bin/kling-guest -listen :8080\n"
	sinEnv := gen + "exec /usr/local/bin/kling-guest -listen :8080\n"
	servicio := gen + ". /etc/kling/env\n( while :; do '/srv'; done ) &\nexec /usr/local/bin/kling-guest -listen :8080\n"
	for _, c := range []struct {
		name, ep string
		oci      bool
		exec     string
		pw       bool // PW de /etc/kling/env en el entorno
	}{
		{"generated with env", conEnv, false, "/usr/local/bin/kling-guest", true},
		{"generated without env", sinEnv, false, "/usr/local/bin/kling-guest", false},
		{"with a service", servicio, false, "/entrypoint", false},
		{"other", "#!/bin/sh\nexec /usr/bin/mcp\n", false, "/entrypoint", false},
		{"none", "", false, "/usr/local/bin/kling-guest", true},
		{"oci ignores it", "#!/bin/sh\nexec /usr/bin/mcp\n", true, "/usr/local/bin/kling-guest", true},
	} {
		f := newFakeInit()
		if c.ep != "" {
			f.files["/entrypoint"] = c.ep
		}
		if c.oci {
			f.files[ociMarker] = "{}"
		}
		f.files[guestEnvPath] = "export PW='x'\n"
		if err := runInit(f, []string{"TERM=linux"}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if last := f.calls[len(f.calls)-1]; last != "exec "+c.exec {
			t.Fatalf("%s: last call %q", c.name, last)
		}
		env := strings.Join(f.execEv, " ")
		if !strings.Contains(env, "PATH="+initPATH) || !strings.Contains(env, "HOME=/root") || !strings.Contains(env, "TERM=linux") {
			t.Fatalf("%s: env %q", c.name, env)
		}
		if strings.Contains(env, "PW=x") != c.pw {
			t.Fatalf("%s: env %q, want PW %v", c.name, env, c.pw)
		}
	}

	// Un /entrypoint que no se puede ejecutar: el agente, no un init muerto.
	f := newFakeInit()
	f.files["/entrypoint"] = "#!/bin/sh\nexec /usr/bin/mcp\n"
	f.fail["exec /entrypoint"] = errors.New("ENOEXEC")
	if err := runInit(f, nil); err != nil || f.calls[len(f.calls)-1] != "exec /usr/local/bin/kling-guest" {
		t.Fatalf("unexecutable /entrypoint: %v %q", err, f.calls[len(f.calls)-1])
	}
}

func TestPlainEntrypointRechaza(t *testing.T) {
	base := "#!/bin/sh\n# x\nexport PATH=" + initPATH + "\nexport HOME=/root\nexec /usr/local/bin/kling-guest -listen :8080\n"
	if _, ok := PlainEntrypoint([]byte(base)); !ok {
		t.Fatal("the plain one is not recognized")
	}
	for _, b := range []string{
		strings.Replace(base, "# x", "# $(evil)", 1),
		strings.Replace(base, "# x", "echo x", 1),
		strings.Replace(base, ":8080", ":9090", 1),
		base + "echo after\n",
		strings.Replace(base, "export HOME=/root\n", "export HOME=/root\n. /etc/other\n", 1),
		strings.Repeat("#", maxEntrypointGenerado+1),
	} {
		if _, ok := PlainEntrypoint([]byte(b)); ok {
			t.Errorf("recognized %q", b)
		}
	}
}

// La disposición de los ioctl: struct dm_ioctl de 312 bytes, cada
// dm_target_spec alineada a 8 y con next relativo a ella.
func TestDMTableLoadBuf(t *testing.T) {
	ts := []dmTarget{{0, 8, "verity", "1 /dev/vdc"}, {8, 16, "linear", "/dev/vdd 0"}}
	b, err := dmTableLoadBuf("kindling-layer", ts)
	if err != nil {
		t.Fatal(err)
	}
	ne := binary.NativeEndian
	if ne.Uint32(b[0:]) != 4 || int(ne.Uint32(b[12:])) != len(b) || ne.Uint32(b[16:]) != 312 ||
		ne.Uint32(b[20:]) != 2 || ne.Uint32(b[28:]) != dmReadonlyFlag {
		t.Fatalf("header %v", b[:32])
	}
	if got := strings.TrimRight(string(b[48:48+128]), "\x00"); got != "kindling-layer" {
		t.Fatalf("name %q", got)
	}
	sp := b[312:]
	next := ne.Uint32(sp[20:])
	if next%8 != 0 || next != 56 || ne.Uint64(sp[8:]) != 8 || strings.TrimRight(string(sp[24:40]), "\x00") != "verity" ||
		string(sp[40:50]) != "1 /dev/vdc" || sp[50] != 0 {
		t.Fatalf("first spec %v", sp[:next])
	}
	sp2 := sp[next:]
	if ne.Uint64(sp2[0:]) != 8 || ne.Uint64(sp2[8:]) != 16 || string(sp2[40:50]) != "/dev/vdd 0" || 312+int(next)+int(ne.Uint32(sp2[20:])) != len(b) {
		t.Fatalf("second spec %v", sp2)
	}
	for _, bad := range []string{"", "a/b", strings.Repeat("x", 128)} {
		if _, err := dmHeader(bad, 0, 0, 0); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if dmDevCreate != 0xc138fd03 || dmTableLoad != 0xc138fd09 || dmDevSuspend != 0xc138fd06 {
		t.Fatalf("ioctl numbers %#x %#x %#x", dmDevCreate, dmTableLoad, dmDevSuspend)
	}
	// huge_encode_dev de 253:0 y de 253:300.
	if ma, mi := dmDecodeDev(253 << 8); ma != 253 || mi != 0 {
		t.Fatalf("253:0 -> %d:%d", ma, mi)
	}
	if ma, mi := dmDecodeDev(300&0xff | 253<<8 | (300&^0xff)<<12); ma != 253 || mi != 300 {
		t.Fatalf("253:300 -> %d:%d", ma, mi)
	}
}
