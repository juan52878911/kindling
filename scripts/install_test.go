package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// install.sh es lo primero que corre quien instala kling y nadie lo ejecutaba
// en CI. Estos tests lo corren de verdad con un curl falso que sirve una
// release desde un directorio y un minisign falso que da la firma por buena
// solo si el SHA256SUMS es el que se firmó, en un HOME y un PATH de usar y
// tirar. Comprueban que con cualquier pieza que no cuadre no se instala nada,
// y que con todo en regla sí.

const tagInstall = "v1.0.0"

// releaseFalsa describe la release que sirve el curl falso.
type releaseFalsa struct {
	tagFirmado   string // el tag: del comentario de confianza de la firma
	sinFirma     bool   // sin SHA256SUMS.minisig
	sumaAlterada bool   // SHA256SUMS cambiado (con su binario) después de firmar
	binAlterado  bool   // el binario no es el de SHA256SUMS
	sinMinisign  bool   // minisign no está en el PATH
	minKling     string // min_kling del manifiesto de kling-mcp
	klingPATH    string // si no vacío, un kling en el PATH que dice esta versión
}

type resultadoInstall struct {
	salida  string
	ok      bool
	prefijo string
	plugins string
}

func plataformaInstall(t *testing.T) string {
	t.Helper()
	var os_ string
	switch runtime.GOOS {
	case "linux", "darwin":
		os_ = runtime.GOOS
	default:
		t.Skip("install.sh solo instala en Linux y macOS")
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		t.Skip("arquitectura que install.sh no conoce")
	}
	return os_ + "-" + runtime.GOARCH
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// correrInstall monta la release falsa y corre install.sh con args.
func correrInstall(t *testing.T, r releaseFalsa, args ...string) resultadoInstall {
	t.Helper()
	plat := plataformaInstall(t)
	raiz := t.TempDir()
	home := filepath.Join(raiz, "home")
	falsos := filepath.Join(raiz, "bin")
	datos := filepath.Join(raiz, "release")
	dirTag := filepath.Join(datos, tagInstall)
	for _, d := range []string{home, falsos, dirTag} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Los binarios de la release: scripts que dicen su versión.
	binarios := map[string]string{
		"kling-" + plat:        "#!/bin/sh\necho 'kling " + tagInstall + "'\n",
		"kling-mcp-" + plat:    "#!/bin/sh\n[ \"$1\" = --kling-manifest ] && echo '{\"name\":\"mcp\",\"min_kling\": \"" + r.minKling + "\"}'\nexit 0\n",
		"kling-bridge-" + plat: "#!/bin/sh\nexit 0\n",
	}
	if plat == "darwin-arm64" {
		binarios["kling-vz-darwin-arm64"] = "#!/bin/sh\nexit 0\n"
	}
	var sums strings.Builder
	for n, c := range binarios {
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex([]byte(c)), n)
	}
	firmado := sums.String()
	sumsFinal := firmado
	if r.sumaAlterada {
		// Binario y suma cambiados a la vez: el sha256 cuadra, la firma no.
		otro := "#!/bin/sh\necho otro\n"
		binarios["kling-"+plat] = otro
		sumsFinal = ""
		for n, c := range binarios {
			sumsFinal += sha256Hex([]byte(c)) + "  " + n + "\n"
		}
	}
	if r.binAlterado {
		binarios["kling-"+plat] = "#!/bin/sh\necho alterado\n"
	}
	for n, c := range binarios {
		escribir(t, filepath.Join(dirTag, n), c, 0o755)
	}
	escribir(t, filepath.Join(dirTag, "SHA256SUMS"), sumsFinal, 0o644)
	if !r.sinFirma {
		tag := r.tagFirmado
		if tag == "" {
			tag = tagInstall
		}
		// Línea 1: el sha256 del SHA256SUMS firmado; línea 2: el comentario
		// de confianza con sus campos separados por tabuladores.
		escribir(t, filepath.Join(dirTag, "SHA256SUMS.minisig"),
			sha256Hex([]byte(firmado))+"\ntimestamp:1\tfile:SHA256SUMS\ttag:"+tag+"\n", 0o644)
	}

	// curl falso: -o DESTINO URL, sirve releases/download/<tag>/<nombre>.
	escribir(t, filepath.Join(falsos, "curl"), `#!/bin/sh
dest=""; url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) dest="$2"; shift 2 ;;
        -w|--retry) shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
f="`+datos+`/${url##*/releases/download/}"
[ -f "$f" ] || exit 22
cp "$f" "$dest"
`, 0o755)
	if !r.sinMinisign {
		escribir(t, filepath.Join(falsos, "minisign"), `#!/bin/sh
m=""; x=""
while [ $# -gt 0 ]; do
    case "$1" in
        -m) m="$2"; shift 2 ;;
        -x) x="$2"; shift 2 ;;
        -P) shift 2 ;;
        *) shift ;;
    esac
done
if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$m" | awk '{print $1}')"
else got="$(shasum -a 256 "$m" | awk '{print $1}')"; fi
[ "$(sed -n 1p "$x")" = "$got" ] || { echo "Signature verification failed" >&2; exit 1; }
echo "Signature and comment signature verified"
echo "Trusted comment: $(sed -n 2p "$x")"
`, 0o755)
	}
	if r.klingPATH != "" {
		escribir(t, filepath.Join(falsos, "kling"), "#!/bin/sh\necho 'kling "+r.klingPATH+"'\n", 0o755)
	}

	res := resultadoInstall{
		prefijo: filepath.Join(home, ".local", "bin"),
		plugins: filepath.Join(home, "plugins"),
	}
	todos := append([]string{filepath.Join("install.sh"), "--tag", tagInstall,
		"--prefix", res.prefijo, "--plugin-dir", res.plugins, "--no-rc"}, args...)
	cmd := exec.Command("/bin/sh", todos...)
	cmd.Env = []string{"HOME=" + home, "PATH=" + falsos + ":/usr/bin:/bin", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	res.salida = string(out)
	res.ok = err == nil
	return res
}

func existe(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestInstallBuenoInstala(t *testing.T) {
	r := correrInstall(t, releaseFalsa{})
	if !r.ok {
		t.Fatalf("una release en regla no se instaló:\n%s", r.salida)
	}
	if !existe(filepath.Join(r.prefijo, "kling")) {
		t.Fatalf("salió con 0 pero no dejó kling en %s:\n%s", r.prefijo, r.salida)
	}
}

func TestInstallRechazaYNoInstalaNada(t *testing.T) {
	// Cada caso con el motivo que tiene que dar: que falle por otra cosa
	// (el curl falso, una herramienta que falta) no prueba nada.
	type caso struct {
		rel    releaseFalsa
		motivo string
	}
	casos := map[string]caso{
		"firma de otra etiqueta": {releaseFalsa{tagFirmado: "v0.9.0"}, "no está firmado para " + tagInstall},
		"sin SHA256SUMS.minisig": {releaseFalsa{sinFirma: true}, "no trae SHA256SUMS.minisig"},
		"SHA256SUMS alterado":    {releaseFalsa{sumaAlterada: true}, "la firma de SHA256SUMS no es válida"},
		"binario alterado":       {releaseFalsa{binAlterado: true}, "no coincide"},
	}
	if !existe("/usr/bin/minisign") && !existe("/bin/minisign") {
		casos["sin minisign"] = caso{releaseFalsa{sinMinisign: true}, "no encuentro minisign"}
	}
	for nombre, c := range casos {
		t.Run(nombre, func(t *testing.T) {
			r := correrInstall(t, c.rel)
			if r.ok {
				t.Fatalf("instaló con %s:\n%s", nombre, r.salida)
			}
			if !strings.Contains(r.salida, c.motivo) {
				t.Fatalf("con %s falló, pero no por %q:\n%s", nombre, c.motivo, r.salida)
			}
			if ents, _ := os.ReadDir(r.prefijo); len(ents) > 0 {
				t.Fatalf("con %s dejó ficheros en %s: %v", nombre, r.prefijo, ents)
			}
		})
	}
}

// Con --skip-kling, el kling del PATH tiene que cumplir el min_kling de la
// extensión, y una prerelease es menor que su versión final.
func TestInstallMinKlingConPrerelease(t *testing.T) {
	casos := []struct {
		tiene, min string
		ok         bool
	}{
		{"v1.0.0-rc.2", "1.0.0", false},
		{"v1.0.0", "1.0.0", true},
		{"v1.0.1-rc.1", "1.0.0", true},
		{"v0.20.1", "1.0.0", false},
		{"v1.0.0-rc.10", "1.0.0-rc.9", true},
		{"v1.0.0-rc.2", "1.0.0-rc.3", false},
		{"v1.0.0+abc", "1.0.0", true},
	}
	for _, c := range casos {
		t.Run(c.tiene+">="+c.min, func(t *testing.T) {
			r := correrInstall(t, releaseFalsa{minKling: c.min, klingPATH: c.tiene},
				"--skip-kling", "--with", "mcp")
			instalada := existe(filepath.Join(r.plugins, "kling-mcp"))
			if r.ok != c.ok || instalada != c.ok {
				t.Fatalf("kling %s con min_kling %s: salió ok=%v, instalada=%v; esperaba %v:\n%s",
					c.tiene, c.min, r.ok, instalada, c.ok, r.salida)
			}
		})
	}
}
