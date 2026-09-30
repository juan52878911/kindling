package ext4

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2fsck busca el de e2fsprogs (en un Mac, el de Homebrew). Sin él, las
// pruebas comprueban la imagen solo releyéndola.
func e2fsck(t *testing.T) string {
	for _, c := range []string{"e2fsck", "/sbin/e2fsck", "/usr/sbin/e2fsck", "/opt/homebrew/opt/e2fsprogs/sbin/e2fsck", "/usr/local/opt/e2fsprogs/sbin/e2fsck"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

func fsck(t *testing.T, img string) {
	t.Helper()
	bin := e2fsck(t)
	if bin == "" {
		t.Log("e2fsck not found: skipping the fsck check")
		return
	}
	out, err := exec.Command(bin, "-fn", img).CombinedOutput()
	if err != nil || strings.Contains(string(out), "? no") {
		t.Fatalf("e2fsck -fn: %v\n%s", err, out)
	}
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func sampleTree(t *testing.T, big int) (*Node, map[string][]byte) {
	now := time.Unix(1700000000, 123456789)
	root := NewDir(0o755, 0, 0, now)
	want := map[string][]byte{}
	add := func(p string, data []byte, mode uint32, uid, gid uint32) *Node {
		n := &Node{Mode: ModeReg | mode, UID: uid, GID: gid, Mtime: now, Size: int64(len(data)), Data: Bytes(data)}
		if err := root.Put(p, n, now); err != nil {
			t.Fatal(err)
		}
		want[p] = data
		return n
	}
	add("/empty", nil, 0o644, 0, 0)
	add("/small", []byte("hola\n"), 0o644, 1000, 1000)
	add("/system/bin/su", pattern(12345, 1), 0o4755|0o2000, 0, 2000)
	cap := add("/system/bin/ping", pattern(9000, 2), 0o755, 0, 0)
	cap.Xattrs = map[string][]byte{"security.capability": append([]byte{0, 0, 0, 2, 0, 0x20, 0, 0}, make([]byte, 12)...)}
	bigx := add("/bigxattr", []byte("x"), 0o600, 0, 0)
	bigx.Xattrs = map[string][]byte{"user.a": bytes.Repeat([]byte("A"), 300), "trusted.b": []byte("b"), "security.selinux": []byte("u:object_r:system_file:s0\x00")}
	holes := make([]byte, 5*blockSize+17)
	copy(holes[2*blockSize:], "medio")
	add("/holes", holes, 0o644, 0, 0)
	add("/deep/a/b/c/d/file", pattern(blockSize*3, 3), 0o640, 5, 6)
	if big > 0 {
		add("/big", pattern(big, 4), 0o644, 0, 0)
	}
	h := root.Lookup("/small")
	if err := root.Put("/hardlink", h, now); err != nil {
		t.Fatal(err)
	}
	want["/hardlink"] = want["/small"]
	root.Put("/lnk", &Node{Mode: ModeLink | 0o777, Target: "system/bin/su", Mtime: now}, now)
	root.Put("/longlnk", &Node{Mode: ModeLink | 0o777, Target: "/" + strings.Repeat("x/", 60) + "end", Mtime: now}, now)
	root.Put("/dev/null", &Node{Mode: ModeChar | 0o666, Major: 1, Minor: 3, Mtime: now}, now)
	root.Put("/dev/big", &Node{Mode: ModeBlock | 0o660, Major: 259, Minor: 300, Mtime: now}, now)
	root.Put("/fifo", &Node{Mode: ModeFIFO | 0o600, Mtime: now}, now)
	d, _ := root.MkdirAll("/many", 0o755, 0, 0, now)
	for i := 0; i < 700; i++ {
		d.SetChild(fmt.Sprintf("entry-with-a-long-name-%04d", i), &Node{Mode: ModeReg | 0o644, Mtime: now})
	}
	return root, want
}

func TestWriteReadFsck(t *testing.T) {
	root, want := sampleTree(t, 0)
	img := filepath.Join(t.TempDir(), "t.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Write(f, root, nil, Options{LostFound: true, ZeroHoles: true, SlackBlocks: 100, Label: "prueba"})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", st)
	fsck(t, img)
	checkBack(t, img, want)
}

func TestWriteBigMultiGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~300 MiB")
	}
	// Más de 4 grupos y un fichero que cruza copias del superbloque: árbol
	// de extents con hojas.
	root, want := sampleTree(t, 5*blocksPerGroup*blockSize+999)
	img := filepath.Join(t.TempDir(), "t.ext4")
	f, _ := os.Create(img)
	st, err := Write(f, root, nil, Options{LostFound: true, ZeroHoles: true})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", st)
	fsck(t, img)
	checkBack(t, img, want)
}

func TestStreams(t *testing.T) {
	now := time.Unix(1700000000, 0)
	root := NewDir(0o755, 0, 0, now)
	data := map[int][]byte{}
	for i := 0; i < 50; i++ {
		data[i] = pattern(i*1000+1, byte(i))
		root.Put(fmt.Sprintf("/s/%02d", i), &Node{Mode: ModeReg | 0o644, Mtime: now, Size: int64(len(data[i])), Data: StreamKey{0, i}}, now)
	}
	stream := func(emit func(int, io.Reader) error) error {
		for i := 0; i < 60; i++ { // 50..59 no están en el árbol
			d := data[i]
			if d == nil {
				d = []byte("nadie")
			}
			if err := emit(i, bytes.NewReader(d)); err != nil {
				return err
			}
		}
		return nil
	}
	img := filepath.Join(t.TempDir(), "s.ext4")
	f, _ := os.Create(img)
	if _, err := Write(f, root, []Stream{stream}, Options{}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	want := map[string][]byte{}
	for i, d := range data {
		want[fmt.Sprintf("/s/%02d", i)] = d
	}
	fsck(t, img)
	checkBack(t, img, want)
}

func TestEmptyBig(t *testing.T) {
	// El data.ext4 de DATA_MODE=tmpfs: 2 GiB vacío y disperso.
	root := NewDir(0o755, 0, 0, time.Now())
	img := filepath.Join(t.TempDir(), "d.ext4")
	f, _ := os.Create(img)
	st, err := Write(f, root, nil, Options{LostFound: true, MinBlocks: 2 << 30 / blockSize, InodeRatio: 16384, Label: "data"})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st.Bytes() != 2<<30 || st.Inodes < 131072 {
		t.Fatalf("stats %+v", st)
	}
	fsck(t, img)
}

func checkBack(t *testing.T, img string, want map[string][]byte) {
	t.Helper()
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	root, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	for p, w := range want {
		n := root.Lookup(p)
		if n == nil {
			t.Fatalf("%s missing after reading back", p)
		}
		got, err := n.ReadAll()
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if sha256.Sum256(got) != sha256.Sum256(w) {
			t.Fatalf("%s: content differs (%d vs %d bytes)", p, len(got), len(w))
		}
	}
	if n := root.Lookup("/system/bin/su"); n != nil && n.Mode != ModeReg|0o6755 {
		t.Fatalf("su mode %o", n.Mode)
	}
	if n := root.Lookup("/system/bin/su"); n != nil && (n.GID != 2000) {
		t.Fatalf("su gid %d", n.GID)
	}
	if n := root.Lookup("/system/bin/ping"); n != nil && len(n.Xattrs["security.capability"]) != 20 {
		t.Fatalf("ping xattrs %v", n.Xattrs)
	}
	if n := root.Lookup("/bigxattr"); n != nil && (len(n.Xattrs["user.a"]) != 300 || string(n.Xattrs["trusted.b"]) != "b") {
		t.Fatalf("bigxattr %v", n.Xattrs)
	}
	if n := root.Lookup("/longlnk"); n != nil && !strings.HasSuffix(n.Target, "end") {
		t.Fatalf("longlnk %q", n.Target)
	}
	if n := root.Lookup("/dev/big"); n != nil && (n.Major != 259 || n.Minor != 300) {
		t.Fatalf("dev %d:%d", n.Major, n.Minor)
	}
	if n := root.Lookup("/small"); n != nil && n.Mtime.Nanosecond() != 123456789 {
		t.Fatalf("mtime %v", n.Mtime)
	}
	if n := root.Lookup("/hardlink"); n != nil && n != root.Lookup("/small") {
		t.Fatal("hard link not shared")
	}
}
