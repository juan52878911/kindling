package imagen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/pkg/api"
)

func TestEmulable(t *testing.T) {
	if p, ok := Emulable("arm64", "amd64"); !ok || p.Member != "./usr/bin/qemu-x86_64" || len(p.SHA256) != 64 || len(p.Deb.SHA256) != 64 {
		t.Fatalf("arm64/amd64: %+v %v", p, ok)
	}
	for _, c := range [][2]string{{"amd64", "arm64"}, {"arm64", "arm64"}, {"arm64", "riscv64"}} {
		if _, ok := Emulable(c[0], c[1]); ok {
			t.Errorf("%s on %s should not be emulable", c[1], c[0])
		}
	}
}

// debCon es un .deb mínimo cuyo data.tar.gz trae files.
func debCon(files map[string][]byte) []byte {
	var tb bytes.Buffer
	zw := gzip.NewWriter(&tb)
	tw := tar.NewWriter(zw)
	for n, b := range files {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(b))})
		tw.Write(b)
	}
	tw.Close()
	zw.Close()
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	member := func(n string, b []byte) {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", n, 0, 0, 0, "100644", len(b))
		out.Write(b)
		if len(b)%2 == 1 {
			out.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("data.tar.gz", tb.Bytes())
	return out.Bytes()
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestQemu(t *testing.T) {
	bin := make([]byte, 4096)
	copy(bin, "\x7fELF\x02\x01\x01")
	bin[18] = 0xb7 // aarch64: el intérprete corre nativo en arm64
	x86 := bytes.Clone(bin)
	x86[18] = 0x3e
	debs := map[string][]byte{
		"/ok.deb":   debCon(map[string][]byte{"./usr/bin/qemu-x86_64": bin, "./usr/bin/otro": []byte("x")}),
		"/x86.deb":  debCon(map[string][]byte{"./usr/bin/qemu-x86_64": x86}),
		"/none.deb": debCon(map[string][]byte{"./usr/bin/otro": []byte("x")}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := debs[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	pin := func(deb string, sum string) QemuPin {
		return QemuPin{Deb: DebPin{Name: "qemu-user", Version: "1", URL: srv.URL + deb, SHA256: hexSum(debs[deb]), Size: int64(len(debs[deb]))},
			Member: "./usr/bin/qemu-x86_64", SHA256: sum, Size: int64(len(bin))}
	}
	d := Debs{Cache: t.TempDir()}
	ctx := context.Background()

	e, err := d.Qemu(ctx, pin("/ok.deb", hexSum(bin)), "arm64", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.Bin, bin) || e.Guest != "amd64" {
		t.Fatalf("emulador: %+v", e.Guest)
	}
	for _, c := range []struct {
		name string
		pin  QemuPin
		want string
	}{
		{"sha256 of the binary", pin("/ok.deb", strings.Repeat("0", 64)), "sha256"},
		{"interpreter of another arch", pin("/x86.deb", hexSum(x86)), "is amd64, not arm64"},
		{"member missing", pin("/none.deb", hexSum(bin)), "has no ./usr/bin/qemu-x86_64"},
	} {
		if _, err := d.Qemu(ctx, c.pin, "arm64", "amd64"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}

	tr := time.Unix(1700000000, 0)
	root := ext4.NewDir(0o755, 0, 0, tr)
	if err := PutEmulador(root, e, tr); err != nil {
		t.Fatal(err)
	}
	n := root.Lookup(api.GuestEmulators["amd64"])
	if n == nil || !n.IsReg() || n.Mode&0o777 != 0o755 || n.Size != int64(len(bin)) {
		t.Fatalf("interpreter: %+v", n)
	}
	m := root.Lookup(api.GuestEmulateFile)
	if m == nil || string(m.Data.(ext4.Bytes)) != "amd64\n" {
		t.Fatalf("marker: %+v", m)
	}
}
