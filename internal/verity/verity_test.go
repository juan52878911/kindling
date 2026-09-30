package verity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testFile(t *testing.T, blocks int) *os.File {
	f, err := os.Create(filepath.Join(t.TempDir(), "img"))
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, blocks*BlockSize)
	x := uint32(7)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	// Unos bloques a cero, como los huecos de un ext4.
	clear(b[5*BlockSize : 9*BlockSize])
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLevels(t *testing.T) {
	// Los números de verity.sh del prototipo: HASH_BLOCKS como suma de
	// niveles de 128 hashes.
	for _, c := range []struct{ n, want uint64 }{{1, 1}, {128, 1}, {129, 3}, {16384, 129}, {16385, 132}, {655304, 5161}} {
		if got := HashBlocks(c.n); got != c.want {
			t.Errorf("HashBlocks(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestAppendVerify(t *testing.T) {
	f := testFile(t, 20000)
	defer f.Close()
	salt := sha256.Sum256([]byte("sal"))
	res, err := Append(f, 20000, salt[:], 2)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if uint64(st.Size()) != res.TotalBlocks()*BlockSize {
		t.Fatalf("size %d, want %d blocks", st.Size(), res.TotalBlocks())
	}
	if err := Verify(f, res); err != nil {
		t.Fatal(err)
	}
	tab := res.Table("@DEV@")
	if !strings.Contains(tab, "verity 1 @DEV@ @DEV@ 4096 4096 20000 20000 sha256 "+hex.EncodeToString(res.RootHash)) ||
		!strings.HasSuffix(tab, "8 use_fec_from_device @DEV@ fec_roots 2 fec_blocks 20160 fec_start 20160") {
		t.Fatalf("table %q", tab)
	}
	// Un bloque de datos cambiado: Verify lo ve.
	if _, err := f.WriteAt([]byte{1}, 3*BlockSize+10); err != nil {
		t.Fatal(err)
	}
	if err := Verify(f, res); err == nil {
		t.Fatal("verify passed on corrupted data")
	}
}

// TestMatchesVeritysetup compara con cryptsetup: el fichero de 3001 bloques
// de testFile con esta sal, pasado por
//
//	veritysetup format --no-superblock --hash sha256 --data-block-size 4096 \
//	  --hash-block-size 4096 --data-blocks 3001 --hash-offset $((3001*4096)) \
//	  --salt 0011...eeff --fec-device F --fec-offset $(((3001+25)*4096)) --fec-roots 2 F F
//
// (cryptsetup 2.7, Ubuntu 24.04 arm64) dio esta raíz y este sha256 del
// fichero entero (datos + árbol + FEC), y `veritysetup verify` con el FEC
// acepta el que escribe Append.
func TestMatchesVeritysetup(t *testing.T) {
	f := testFile(t, 3001)
	defer f.Close()
	salt, _ := hex.DecodeString("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	res, err := Append(f, 3001, salt, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(res.RootHash); got != "fbff359ae8ed2329360f402214601b63f9417a3897b8fa3f99a67c548b02cd76" {
		t.Fatalf("root %s", got)
	}
	if res.HashBlocks != 25 || res.FECBlocks != 24 {
		t.Fatalf("%+v", res)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != "fa22b218af55a1015b0e3c6ddba4178a2422c5a15d2137503b058faf8a4a810f" {
		t.Fatalf("file sha256 %s (differs from veritysetup's)", got)
	}
}
