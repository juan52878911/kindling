package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func adbKey(seed byte, note string) string {
	raw := bytes.Repeat([]byte{seed}, adbPubKeyLen)
	k := base64.StdEncoding.EncodeToString(raw)
	if note != "" {
		k += " " + note
	}
	return k
}

func TestIdentityValidate(t *testing.T) {
	good := `{"phone":{"android_id":"0123456789abcdef","name":"phone-1","serial":"ABC123XYZ","adb_keys":["` +
		adbKey(1, "juan@mac") + `"]}}`
	p, err := parseMMDS([]byte(good))
	if err != nil || p == nil || p.SSAID != "regen" || p.Serial != "ABC123XYZ" {
		t.Fatalf("good doc: %+v %v", p, err)
	}
	if !reSSAIDKey.MatchString(p.SSAIDKey) || p.SSAIDKey != strings.ToUpper(p.SSAIDKey) {
		t.Fatalf("ssaid key %q", p.SSAIDKey)
	}
	k, _ := parseSSAID([]byte(p.ssaidXML()))
	sum := sha256.Sum256([]byte(p.SSAIDKey))
	if k != hex.EncodeToString(sum[:])[:16] {
		t.Fatalf("ssaid xml does not round-trip: %q", p.ssaidXML())
	}
	if !strings.HasSuffix(p.adbKeysFile(), "juan@mac\n") {
		t.Fatalf("adb_keys file %q", p.adbKeysFile())
	}
	// Sin serie: una al azar, distinta cada vez.
	a, _ := parseMMDS([]byte(`{"phone":{"android_id":"0123456789abcdef"}}`))
	b, _ := parseMMDS([]byte(`{"phone":{"android_id":"0123456789abcdef"}}`))
	if a == nil || b == nil || !reSerial.MatchString(a.Serial) || a.Serial == b.Serial {
		t.Fatalf("random serials %v %v", a, b)
	}
	// La huella es la del documento recibido: el mismo documento, la misma
	// (el gancho no regenera en cada thaw), aunque se rellene al azar.
	if a.digest() != b.digest() || a.SSAIDKey == b.SSAIDKey {
		t.Fatal("digest depends on the random fill")
	}
	c, _ := parseMMDS([]byte(`{"phone":{"android_id":"0123456789abcdee"}}`))
	if c.digest() == a.digest() {
		t.Fatal("digest ignores the android_id")
	}
	for _, empty := range []string{``, `{}`, `{"env":{"A":"b"}}`} {
		if p, err := parseMMDS([]byte(empty)); p != nil || err != nil {
			t.Fatalf("%q: %v %v", empty, p, err)
		}
	}
	secret := "0123456789abcdeZ"
	bad := []string{
		`{"phone":{"android_id":"` + secret + `"}}`,
		`{"phone":{"android_id":"0123456789abcdef","name":"a b"}}`,
		`{"phone":{"android_id":"0123456789abcdef","serial":"x;id"}}`,
		`{"phone":{"android_id":"0123456789abcdef","adb_keys":["AAAA"]}}`,
		`{"phone":{"android_id":"0123456789abcdef","adb_keys":["` + adbKey(2, "a\tb") + `"]}}`,
		`{"phone":{"android_id":"0123456789abcdef","ssaid":"maybe"}}`,
		`{"phone":{"android_id":"0123456789abcdef","ssaid_key":"xyz"}}`,
		`{"phone":` + secret,
	}
	for _, d := range bad {
		_, err := parseMMDS([]byte(d))
		if err == nil {
			t.Fatalf("accepted %s", d)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks the value: %v", err)
		}
	}
	var keys []string
	for i := 0; i < maxAdbKeys+1; i++ {
		keys = append(keys, adbKey(byte(i), ""))
	}
	doc, _ := json.Marshal(map[string]any{"phone": map[string]any{"android_id": "0123456789abcdef", "adb_keys": keys}})
	if _, err := parseMMDS(doc); err == nil {
		t.Fatal("too many adb keys accepted")
	}
}

func TestFetchPhoneIdentity(t *testing.T) {
	var sawToken bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT" && r.URL.Path == "/latest/api/token":
			fmt.Fprint(w, "tok")
		case r.Method == "GET" && r.URL.Path == "/":
			sawToken = r.Header.Get("X-metadata-token") == "tok" && r.Header.Get("Accept") == "application/json"
			fmt.Fprint(w, `{"phone":{"android_id":"00000000000000aa"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p, err := fetchPhoneIdentity(t.Context(), srv.URL)
	if err != nil || p == nil || p.AndroidID != "00000000000000aa" || !sawToken {
		t.Fatalf("%+v %v token=%v", p, err, sawToken)
	}
}

func TestParseShellConf(t *testing.T) {
	kv, err := parseShellConf(strings.NewReader(`# comentario
ANDROID_ROOT=/android
ANDROID_EXTRA_ARGS="androidboot.use_redroid_stream=1 androidboot.use_redroid_vnc=1"
ANDROID_NET='isolated'

ANDROID_ADB_SECURE=1
`))
	if err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	if err := c.apply(kv); err != nil {
		t.Fatal(err)
	}
	if c.Net != "isolated" || len(c.ExtraArgs) != 2 || !c.AdbSecure || c.Root != "/android" || c.Listen != ":8091" {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []map[string]string{{"ANDROID_NET": "bridge"}, {"ANDROID_DATA_MODE": "zram"},
		{"ANDROID_PORTS": "5555 x"}, {"ANDROID_ROOT": "android"}} {
		c := defaultConfig()
		if err := c.apply(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if _, err := parseShellConf(strings.NewReader("no equals here\n")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestNetlinkMessages(t *testing.T) {
	le := binary.LittleEndian
	m := msgNewVeth(7, "kandroid0", "eth0", 1234)
	if int(le.Uint32(m)) != len(m) || le.Uint16(m[4:]) != rtmNewLink || le.Uint32(m[8:]) != 7 {
		t.Fatalf("header %x", m[:16])
	}
	if f := le.Uint16(m[6:]); f != nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl {
		t.Fatalf("flags %#x", f)
	}
	if !bytes.Contains(m, []byte("kandroid0\x00")) || !bytes.Contains(m, []byte("veth\x00")) || !bytes.Contains(m, []byte("eth0\x00")) {
		t.Fatal("names missing")
	}
	pid := make([]byte, 4)
	le.PutUint32(pid, 1234)
	if !bytes.Contains(m, append([]byte{8, 0, iflaNetNsPid, 0}, pid...)) {
		t.Fatal("IFLA_NET_NS_PID missing")
	}
	// Los atributos anidados cuadran: longitud de IFLA_LINKINFO = resto del mensaje.
	off := nlmsgHdrLen + 16
	for off < len(m) {
		l := int(le.Uint16(m[off:]))
		if l < 4 || off+l > len(m) {
			t.Fatalf("attr at %d len %d", off, l)
		}
		off += align4(l)
	}
	if off != len(m) {
		t.Fatalf("attrs end at %d of %d", off, len(m))
	}
	a := msgAddAddr(1, 3, net.ParseIP("10.88.0.1"), 30)
	if a[nlmsgHdrLen] != afInet || a[nlmsgHdrLen+1] != 30 || le.Uint32(a[nlmsgHdrLen+4:]) != 3 ||
		!bytes.Contains(a, []byte{10, 88, 0, 1}) {
		t.Fatalf("addr %x", a)
	}
	r := msgDefaultRoute(2, 5, net.ParseIP("10.88.0.1"))
	if r[nlmsgHdrLen+4] != rtTableMain || r[nlmsgHdrLen+7] != rtnUnicast {
		t.Fatalf("route %x", r)
	}
	// ACK: nlmsgerr con errno -17 (EEXIST) para seq 2.
	ack := make([]byte, 36)
	le.PutUint32(ack, 36)
	le.PutUint16(ack[4:], nlmsgError)
	le.PutUint32(ack[8:], 2)
	le.PutUint32(ack[16:], uint32(0xffffffef))
	if e, ok := nlAckErr(ack, 2); !ok || e != -17 {
		t.Fatalf("ack %d %v", e, ok)
	}
	if _, ok := nlAckErr(ack, 3); ok {
		t.Fatal("ack for another seq")
	}
	if _, ok := nlAckErr(ack[:10], 2); ok {
		t.Fatal("short ack parsed")
	}
}

func TestRingLog(t *testing.T) {
	r := newRingLog(64)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(r, "line %02d\n", i)
	}
	if got := string(r.tail(2)); got != "line 18\nline 19\n" {
		t.Fatalf("tail 2 = %q", got)
	}
	if got := r.tail(100); len(got) > 64 || !bytes.HasSuffix(got, []byte("line 19\n")) {
		t.Fatalf("tail all = %q", got)
	}
}

func TestParseSSAID(t *testing.T) {
	xml := `<?xml version='1.0' encoding='utf-8' standalone='yes' ?>
<settings version="-1">
  <setting id="0" name="userkey" value="ABCDEF" package="android" defaultValue="ABCDEF" defaultSysSet="true" />
  <setting id="1" name="10081" value="1122334455667788" package="com.termux" defaultValue="x" defaultSysSet="false" tag="null" />
</settings>`
	key, pk := parseSSAID([]byte(xml))
	if len(key) != 16 || strings.Contains(key, "ABCDEF") || pk["com.termux"] != "1122334455667788" || len(pk) != 1 {
		t.Fatalf("%q %v", key, pk)
	}
}
