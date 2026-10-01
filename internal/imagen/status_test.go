package imagen

import "testing"

func TestDropStatus(t *testing.T) {
	status := "Package: libssl3t64\nStatus: install ok installed\nArchitecture: amd64\nVersion: 3.5.7-1~deb13u2\n\n" +
		"Package: libssl3t64-dev\nStatus: install ok installed\nArchitecture: amd64\nVersion: 1\n\n" +
		"Package: zlib1g\nStatus: install ok installed\nArchitecture: amd64\nDepends: libssl3t64\nVersion: 1\n"
	got := string(dropStatus([]byte(status), "libssl3t64", "amd64"))
	want := "Package: libssl3t64-dev\nStatus: install ok installed\nArchitecture: amd64\nVersion: 1\n\n" +
		"Package: zlib1g\nStatus: install ok installed\nArchitecture: amd64\nDepends: libssl3t64\nVersion: 1\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// Otra arquitectura u otro paquete: igual que estaba.
	for _, c := range [][2]string{{"libssl3t64", "arm64"}, {"openssl", "amd64"}} {
		if got := string(dropStatus([]byte(status), c[0], c[1])); got != status {
			t.Fatalf("%v changed the status:\n%s", c, got)
		}
	}
}
