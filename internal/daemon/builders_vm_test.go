package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// vmFalsa es un invitado en memoria: sus ficheros, y un Exec que entiende solo
// los tres comandos del protocolo (juntar trozos, partir y el constructor).
type vmFalsa struct {
	files   map[string][]byte
	execs   [][]string
	builder func(v *vmFalsa, env []string) (int, string)
	// mentirTamano hace que Size diga otra cosa que lo que Get entrega.
	mentirTamano int64
}

func nuevaVM() *vmFalsa { return &vmFalsa{files: map[string][]byte{}} }

func (v *vmFalsa) Put(_ context.Context, p string, _ os.FileMode, r io.Reader) error {
	b, err := io.ReadAll(r)
	v.files[p] = b
	return err
}

func (v *vmFalsa) Get(_ context.Context, p string) (io.ReadCloser, error) {
	b, ok := v.files[p]
	if !ok {
		return nil, fmt.Errorf("no such file %s", p)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (v *vmFalsa) Size(_ context.Context, p string) (int64, error) {
	if v.mentirTamano != 0 {
		return v.mentirTamano, nil
	}
	b, ok := v.files[p]
	if !ok {
		return 0, fmt.Errorf("no such file %s", p)
	}
	return int64(len(b)), nil
}

func (v *vmFalsa) Exec(_ context.Context, cmd []string) (int, string, error) {
	v.execs = append(v.execs, cmd)
	if cmd[0] == "env" {
		if v.builder != nil {
			e, o := v.builder(v, cmd)
			return e, o, nil
		}
		return 0, "", nil
	}
	script := cmd[2]
	switch {
	case strings.Contains(script, "cat \"$f\""): // juntar trozos
		q := script[strings.Index(script, "for f in ")+len("for f in "):]
		path := strings.Trim(q[:strings.Index(q, ".part*")], "'")
		var names []string
		for n := range v.files {
			if strings.HasPrefix(n, path+".part") {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			v.files[path] = append(v.files[path], v.files[n]...)
			delete(v.files, n)
		}
	case strings.Contains(script, "split -b"): // partir
		var path string
		fmt.Sscanf(script[strings.Index(script, "split -b"):], "split -b %d", new(int64))
		f := strings.Fields(script[strings.Index(script, "split -b"):])
		path = strings.Trim(f[3], "'")
		b := v.files[path]
		for i := 0; len(b) > 0; i++ {
			n := min(int(vmTrozo), len(b))
			suf := string(rune('a'+i/26)) + string(rune('a'+i%26))
			v.files[path+".p."+suf] = b[:n]
			b = b[n:]
		}
		delete(v.files, path)
	default:
		return 1, "unknown", nil
	}
	return 0, "", nil
}

func ponerBlob(t *testing.T, cache string, b []byte) string {
	t.Helper()
	d := digestDe(b)
	p := filepath.Join(cache, "sha256", strings.TrimPrefix(d, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPlanAislado(t *testing.T) {
	good := digestDe([]byte("x"))
	casos := []struct {
		nombre, json string
		ok           bool
	}{
		{"bien", fmt.Sprintf(`{"digest":%q,"epoch":5,"blobs":[%q]}`, good, good), true},
		{"digest malo", fmt.Sprintf(`{"digest":"sha256:../x","blobs":[%q]}`, good), false},
		{"blob con ruta", fmt.Sprintf(`{"digest":%q,"blobs":["sha256:../../etc/passwd"]}`, good), false},
		{"sin blobs", fmt.Sprintf(`{"digest":%q,"blobs":[]}`, good), false},
		{"no es json", `{`, false},
	}
	for _, c := range casos {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "isolate.json"), []byte(c.json), 0o600)
		_, err := leerPlanAislado(d)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.nombre, err)
		}
	}
	// Un enlace no se sigue.
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "real"), []byte(fmt.Sprintf(`{"digest":%q,"blobs":[%q]}`, good, good)), 0o600)
	os.Symlink("real", filepath.Join(d, "isolate.json"))
	if _, err := leerPlanAislado(d); err == nil {
		t.Error("isolate.json as a symlink was followed")
	}
	if _, err := leerPlanAislado(t.TempDir()); err == nil {
		t.Error("missing plan accepted")
	}
}

func TestBlobVerificado(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "oci")
	d := ponerBlob(t, cache, []byte("capa"))
	f, size, err := blobVerificado([]string{cache}, d)
	if err != nil || size != 4 {
		t.Fatalf("blob bueno: %v %d", err, size)
	}
	if b, _ := io.ReadAll(f); string(b) != "capa" {
		t.Errorf("no queda al principio: %q", b)
	}
	f.Close()

	// Contenido que no es su digest (un constructor comprometido lo cambió).
	os.WriteFile(filepath.Join(cache, "sha256", strings.TrimPrefix(d, "sha256:")), []byte("otra"), 0o644)
	if _, _, err := blobVerificado([]string{cache}, d); err == nil {
		t.Error("a blob that doesn't match its digest was accepted")
	}
	// Se busca en la siguiente caché.
	otra := filepath.Join(t.TempDir(), "oci")
	ponerBlob(t, otra, []byte("capa"))
	if _, _, err := blobVerificado([]string{cache, otra}, d); err != nil {
		t.Errorf("second cache: %v", err)
	}
	// Un enlace a otro fichero (aunque su contenido cuadre) no se sigue.
	sl := filepath.Join(t.TempDir(), "oci")
	os.MkdirAll(filepath.Join(sl, "sha256"), 0o755)
	real := filepath.Join(t.TempDir(), "real")
	os.WriteFile(real, []byte("capa"), 0o644)
	os.Symlink(real, filepath.Join(sl, "sha256", strings.TrimPrefix(d, "sha256:")))
	if _, _, err := blobVerificado([]string{sl}, d); err == nil {
		t.Error("a symlinked blob was followed")
	}
}

func conTrozo(t *testing.T, n int64) {
	old := vmTrozo
	vmTrozo = n
	t.Cleanup(func() { vmTrozo = old })
}

func TestSubirEnTrozos(t *testing.T) {
	conTrozo(t, 10)
	datos := []byte(strings.Repeat("0123456789abcdef", 5)) // 80 bytes: 8 trozos
	v := nuevaVM()
	if err := subirFichero(context.Background(), v, "/work/blob", 0o644, bytes.NewReader(datos), int64(len(datos))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(v.files["/work/blob"], datos) {
		t.Errorf("joined blob differs: %d bytes", len(v.files["/work/blob"]))
	}
	for n := range v.files {
		if strings.Contains(n, ".part") {
			t.Errorf("left a part behind: %s", n)
		}
	}
	// Uno chico va de una vez, sin Exec.
	v = nuevaVM()
	subirFichero(context.Background(), v, "/work/small", 0o644, strings.NewReader("hola"), 4)
	if string(v.files["/work/small"]) != "hola" || len(v.execs) != 0 {
		t.Errorf("small upload: %q, %d execs", v.files["/work/small"], len(v.execs))
	}
}

func TestBajarConTope(t *testing.T) {
	conTrozo(t, 10)
	datos := []byte(strings.Repeat("ABCDEFGHIJ", 7)) // 70 bytes
	v := nuevaVM()
	v.files["/work/out/i.ext4"] = datos
	var w bytes.Buffer
	n, err := bajarFichero(context.Background(), v, "/work/out/i.ext4", &w, 100)
	if err != nil || n != 70 || !bytes.Equal(w.Bytes(), datos) {
		t.Fatalf("download in parts: n=%d err=%v", n, err)
	}

	// Sobre el tope: se rechaza sin bajar un byte (una bomba que llenó el disco).
	v = nuevaVM()
	v.files["/work/out/i.ext4"] = datos
	w.Reset()
	if _, err := bajarFichero(context.Background(), v, "/work/out/i.ext4", &w, 50); err == nil || w.Len() != 0 {
		t.Errorf("over the cap: err=%v, wrote %d", err, w.Len())
	}

	// Un invitado que dice un tamaño y entrega más: no se acepta ni se escribe de más.
	v = nuevaVM()
	v.files["/work/out/i.ext4"] = []byte("0123456789012345") // 16, trozos de 10
	v.mentirTamano = 5
	w.Reset()
	if _, err := bajarFichero(context.Background(), v, "/work/out/i.ext4", &w, 100); err == nil {
		t.Error("a guest that sends more than it announced was accepted")
	}
	if w.Len() > 6 {
		t.Errorf("wrote %d bytes past the announced size", w.Len())
	}
}

func cargaDePrueba(t *testing.T, blobs ...[]byte) (cargaVM, []string) {
	cache := filepath.Join(t.TempDir(), "oci")
	var ds []string
	for _, b := range blobs {
		ds = append(ds, ponerBlob(t, cache, b))
	}
	return cargaVM{Name: "pg", Request: []byte(`{"name":"pg"}`),
		Plan:   planAislado{Digest: ds[0], Epoch: 1700000000, Blobs: ds},
		Caches: []string{cache}, MaxImage: 1 << 20}, ds
}

func TestAplanarEnInvitado(t *testing.T) {
	conTrozo(t, 8)
	c, ds := cargaDePrueba(t, []byte("manifiesto"), []byte(strings.Repeat("capa-grande-", 5)))
	v := nuevaVM()
	v.builder = func(v *vmFalsa, cmd []string) (int, string) {
		v.files["/work/out/pg.ext4"] = []byte("EXT4-IMAGE")
		v.files["/work/recipe.json"] = []byte(`{"base":""}`)
		return 0, "image pg ready\n"
	}
	var img, rec bytes.Buffer
	log, err := aplanarEnInvitado(context.Background(), v, c, &img, &rec)
	if err != nil {
		t.Fatal(err)
	}
	if img.String() != "EXT4-IMAGE" || rec.String() != `{"base":""}` || !strings.Contains(log, "ready") {
		t.Errorf("results: %q %q %q", img.String(), rec.String(), log)
	}
	for _, d := range ds {
		if _, ok := v.files["/work/cache/oci/sha256/"+strings.TrimPrefix(d, "sha256:")]; !ok {
			t.Errorf("blob %s did not reach the guest cache", d)
		}
	}
	if string(v.files["/work/request.json"]) != `{"name":"pg"}` {
		t.Error("request.json not sent")
	}
	// El constructor corre en fase flatten, con el digest fijado y sin entorno heredado.
	var env string
	for _, e := range v.execs {
		if e[0] == "env" {
			env = strings.Join(e, " ")
		}
	}
	for _, want := range []string{"env -i", "KLING_OCI_PHASE=flatten", "KLING_OCI_PINNED=" + ds[0],
		"SOURCE_DATE_EPOCH=1700000000", "KLING_OCI_NO_REUSE=1", "builder oci /work"} {
		if !strings.Contains(env, want) {
			t.Errorf("builder command lacks %q: %s", want, env)
		}
	}
}

func TestAplanarFallaDentro(t *testing.T) {
	// El tar hostil hace fallar al constructor dentro de la VM: no sale ext4
	// y el error lleva lo que dijo.
	c, _ := cargaDePrueba(t, []byte("manifiesto"))
	v := nuevaVM()
	v.builder = func(v *vmFalsa, cmd []string) (int, string) {
		return 1, "layer 0 (sha256:x): tar entry \"../../etc/x\" escapes the root\n"
	}
	var img bytes.Buffer
	_, err := aplanarEnInvitado(context.Background(), v, c, &img, nil)
	if err == nil || !strings.Contains(err.Error(), "escapes the root") || img.Len() != 0 {
		t.Errorf("err=%v, image bytes=%d", err, img.Len())
	}

	// Sale bien pero el ext4 pasa del tope: no se escribe.
	v = nuevaVM()
	v.builder = func(v *vmFalsa, cmd []string) (int, string) {
		v.files["/work/out/pg.ext4"] = make([]byte, 2<<20)
		return 0, ""
	}
	c.MaxImage = 1 << 20
	_, err = aplanarEnInvitado(context.Background(), v, c, &img, nil)
	if err == nil || img.Len() != 0 {
		t.Errorf("image over the cap: err=%v, wrote %d", err, img.Len())
	}

	// Un blob que no es su digest no llega a la VM.
	c, ds := cargaDePrueba(t, []byte("manifiesto"))
	os.WriteFile(filepath.Join(c.Caches[0], "sha256", strings.TrimPrefix(ds[0], "sha256:")), []byte("cambiado"), 0o644)
	v = nuevaVM()
	if _, err := aplanarEnInvitado(context.Background(), v, c, &img, nil); err == nil || len(v.files) != 0 {
		t.Errorf("tampered blob: err=%v, %d files sent", err, len(v.files))
	}
}

func TestAislarSpec(t *testing.T) {
	mk := func(b, s string) api.BuildImageRequest { return api.BuildImageRequest{Builder: b, Spec: []byte(s)} }
	if !aislarSpec(mk("oci", `{"ref":"x","isolate":true}`)) {
		t.Error("isolate:true not detected")
	}
	for _, r := range []api.BuildImageRequest{mk("oci", `{"ref":"x"}`), mk("oci", ``), mk("base", `{"isolate":true}`)} {
		if aislarSpec(r) {
			t.Errorf("%s %s detected", r.Builder, r.Spec)
		}
	}
}
