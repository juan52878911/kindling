package imagen

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/scripts"
)

func TestVerityInit(t *testing.T) {
	for _, m := range []Marca{
		{Constructor: "android", Dir: "/etc/kindling-android", DM: "android-layer"},
		{Constructor: "debian", Dir: "/etc/kindling", DM: "kindling-layer"},
	} {
		s, err := VerityInit(scripts.MinimalInit, m)
		if err != nil {
			t.Fatal(err)
		}
		i := strings.Index(s, "dmsetup create "+m.DM+" --readonly")
		j := strings.Index(s, verityAnchor)
		if i < 0 || j < i || strings.Count(s, verityAnchor) != 1 {
			t.Fatalf("%s: the verity block is not right before the layer mount", m.Constructor)
		}
		for _, w := range []string{"if [ -f " + m.VerityConf() + " ]", "LAYER_DEV=/dev/mapper/" + m.DM + "\n", "constructor " + m.Constructor + " de kindling"} {
			if !strings.Contains(s, w) {
				t.Errorf("%s: init lacks %q", m.Constructor, w)
			}
		}
	}
	// El bloque de android, tal cual estaba en internal/android (los bytes
	// de su base no cambian).
	s, _ := VerityInit(scripts.MinimalInit, Marca{Constructor: "android", Dir: "/etc/kindling-android", DM: "android-layer"})
	if !strings.Contains(s, "  # kindling-android: dm-verity (constructor android de kindling). La capa\n") ||
		!strings.Contains(s, `      echo "kindling-android: dm-verity on $LAYER_DEV failed; refusing to mount the layer unverified" >&2`) {
		t.Fatalf("android verity block changed:\n%s", s)
	}
	if _, err := VerityInit("#!/bin/sh\n", Marca{}); err == nil {
		t.Fatal("an init without the anchor was accepted")
	}
}

func TestEntrypoint(t *testing.T) {
	m := Marca{Constructor: "debian"}
	e := Entrypoint(m, []string{"A=it's"}, "")
	if strings.Contains(e, "while :") || strings.Contains(e, "export A") || !strings.Contains(e, ". "+EnvPath+"\n") || !strings.HasSuffix(e, "exec /usr/local/bin/kling-guest -listen :8080\n") {
		t.Fatalf("entrypoint:\n%s", e)
	}
	if f := EnvFile([]string{"A=it's"}); f != `export A='it'\''s'`+"\n" {
		t.Fatalf("env file: %q", f)
	}
	if e := Entrypoint(m, nil, "/usr/bin/svc"); strings.Contains(e, EnvPath) || !strings.Contains(e, "( while :; do '/usr/bin/svc';") {
		t.Fatalf("entrypoint with service:\n%s", e)
	}
}

func TestCheckPin(t *testing.T) {
	ok := DebPin{Name: "python3-minimal", Version: "3.13.5-1", SHA256: strings.Repeat("a", 64), Size: 10,
		URL: "https://deb.debian.org/debian/pool/main/p/python3-defaults/python3-minimal_3.13.5-1_amd64.deb"}
	if err := CheckPin(ok); err != nil {
		t.Fatal(err)
	}
	for i, mut := range []func(*DebPin){
		func(p *DebPin) { p.URL = "http://deb.debian.org/debian/pool/x.deb" },
		func(p *DebPin) { p.URL = "https://169.254.169.254/x.deb" },
		func(p *DebPin) { p.URL = "https://deb.debian.org.evil.com/debian/x.deb" },
		func(p *DebPin) { p.URL = "https://deb.debian.org/debian/../x.deb" },
		func(p *DebPin) { p.URL = "https://deb.debian.org/debian/pool/x.deb?y" },
		func(p *DebPin) { p.SHA256 = "abc" },
		func(p *DebPin) { p.Size = 0 },
		func(p *DebPin) { p.Name = "Bad Name" },
		func(p *DebPin) { p.Version = "1 2" },
	} {
		p := ok
		mut(&p)
		if err := CheckPin(p); err == nil {
			t.Errorf("case %d accepted: %+v", i, p)
		}
	}
}

func TestCheckELF(t *testing.T) {
	b := make([]byte, 64)
	copy(b, "\x7fELF\x02\x01\x01")
	b[18] = 0x3e
	if CheckELF(b, "amd64") != nil || CheckELF(b, "arm64") == nil || CheckELF([]byte("#!/bin/sh"), "amd64") == nil {
		t.Fatal("CheckELF")
	}
}
