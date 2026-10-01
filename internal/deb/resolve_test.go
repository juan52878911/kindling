package deb

import (
	"strings"
	"testing"
)

const testIndex = `Package: python3-minimal
Version: 3.13.5-1
Pre-Depends: python3.13-minimal (>= 3.13.5-1~)
Depends: dpkg (>= 1.13.20)
Filename: pool/main/p/python3-defaults/python3-minimal_3.13.5-1_amd64.deb
Size: 100
SHA256: aa

Package: python3.13-minimal
Version: 3.13.5-2
Depends: libpython3.13-minimal (= 3.13.5-2), libexpat1 (>= 2.1~beta3), zlib1g (>= 1:1.2.0)
Filename: pool/main/p/python3.13/python3.13-minimal_3.13.5-2_amd64.deb
Size: 200
SHA256: bb

Package: python3.13-minimal
Version: 3.13.5-1
Filename: pool/main/p/python3.13/python3.13-minimal_3.13.5-1_amd64.deb
Size: 1
SHA256: old

Package: libpython3.13-minimal
Version: 3.13.5-2
Depends: libc6 (>= 2.38), libssl3t64 | libssl-virtual
Filename: pool/main/p/python3.13/libpython3.13-minimal_3.13.5-2_amd64.deb
Size: 300
SHA256: cc

Package: libexpat1
Version: 2.7.1-2
Depends: libc6 (>= 2.25)
Filename: pool/main/e/expat/libexpat1_2.7.1-2_amd64.deb
Size: 50
SHA256: dd

Package: openssl-provider
Version: 1
Provides: libssl-virtual
Filename: pool/main/o/x.deb
Size: 1
SHA256: ee
`

const testStatus = `Package: libc6
Status: install ok installed
Version: 2.41-12

Package: zlib1g
Status: install ok installed
Version: 1:1.3.dfsg+really1.3.1-1+b1
Provides: libz1

Package: libssl3t64
Status: deinstall ok config-files
Version: 3.5.1-1

Package: dpkg
Status: install ok installed
Version: 1.22.21
`

func TestResolve(t *testing.T) {
	ix := NewIndex()
	if err := ix.Add(strings.NewReader(testIndex), "https://deb.debian.org/debian/"); err != nil {
		t.Fatal(err)
	}
	inst, err := Installed(strings.NewReader(testStatus))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inst["libz1"]; !ok {
		t.Fatal("Provides of an installed package not counted")
	}
	got, err := ix.Resolve([]string{"python3-minimal"}, inst)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name+"="+p.Version)
	}
	// libssl3t64 no cuenta como instalado (solo quedan sus conffiles): la
	// alternativa que hay en el índice es el paquete virtual.
	want := "libexpat1=2.7.1-2 libpython3.13-minimal=3.13.5-2 openssl-provider=1 python3-minimal=3.13.5-1 python3.13-minimal=3.13.5-2"
	if strings.Join(names, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(names, " "), want)
	}
	if got[0].Base != "https://deb.debian.org/debian" || got[0].Filename != "pool/main/e/expat/libexpat1_2.7.1-2_amd64.deb" || got[0].SHA256 != "dd" || got[0].Size != 50 {
		t.Fatalf("%+v", got[0])
	}
	if _, err := ix.Resolve([]string{"nonexistent"}, inst); err == nil {
		t.Fatal("resolved a package that is not in the archive")
	}
	if got, _ := ix.Resolve([]string{"libc6"}, inst); len(got) != 0 {
		t.Fatal("re-added an installed package")
	}
}

func TestUpgrades(t *testing.T) {
	ix := NewIndex()
	if err := ix.Add(strings.NewReader(testIndex), "https://deb.debian.org/debian"); err != nil {
		t.Fatal(err)
	}
	// python3.13-minimal tiene una versión más alta en el índice, libexpat1
	// ya está al día y lo que no está instalado no se actualiza.
	status := `Package: python3.13-minimal
Status: install ok installed
Version: 3.13.5-1

Package: libexpat1
Status: install ok installed
Version: 2.7.1-2

Package: libpython3.13-minimal
Status: deinstall ok config-files
Version: 3.13.5-1
`
	got, err := ix.Upgrades(strings.NewReader(status))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "python3.13-minimal" {
		t.Fatalf("upgrades = %v", got)
	}
}
