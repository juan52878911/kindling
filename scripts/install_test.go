package scripts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/upgrade"
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
	tag          string // la etiqueta de la release; vacía, tagInstall
	latest       bool   // sin --tag: install.sh la resuelve por /releases/latest
	tagFirmado   string // el tag: del comentario de confianza de la firma
	sinFirma     bool   // sin SHA256SUMS.minisig
	sumaAlterada bool   // SHA256SUMS cambiado (con su binario) después de firmar
	binAlterado  bool   // el binario no es el de SHA256SUMS
	sinMinisign  bool   // minisign no está en el PATH
	minKling     string // min_kling del manifiesto de kling-mcp
	klingPATH    string // si no vacío, un kling en el PATH que dice esta versión
	conWget      bool   // sin curl en el PATH: baja con un wget falso
	plat         string // si no vacío, la plataforma que dice un uname falso (darwin-arm64)
	sinGuest     bool   // la release no publica kling-guest-linux-<arch>
	klingReal    string // si no vacío, ruta de un kling de verdad que sirve como binario de la release
	// firmaMinisig y firmaSSH, si no nil, dan el contenido de SHA256SUMS.minisig
	// y SHA256SUMS.sshsig a partir del SHA256SUMS firmado y la etiqueta.
	firmaMinisig func(sums, tag string) []byte
	firmaSSH     func(sums, tag string) []byte
	env          []string // variables extra para install.sh
}

type resultadoInstall struct {
	salida  string
	ok      bool
	prefijo string
	plugins string
}

func plataformaInstall(t *testing.T, forzada string) string {
	t.Helper()
	if forzada != "" {
		return forzada
	}
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
	plat := plataformaInstall(t, r.plat)
	raiz := t.TempDir()
	home := filepath.Join(raiz, "home")
	falsos := filepath.Join(raiz, "bin")
	datos := filepath.Join(raiz, "release")
	tagRel := r.tag
	if tagRel == "" {
		tagRel = tagInstall
	}
	dirTag := filepath.Join(datos, tagRel)
	for _, d := range []string{home, falsos, dirTag} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Los binarios de la release: scripts que dicen su versión.
	binarios := map[string]string{
		"kling-" + plat:        klingFalso(tagRel),
		"kling-mcp-" + plat:    "#!/bin/sh\n[ \"$1\" = --kling-manifest ] && echo '{\"name\":\"mcp\",\"min_kling\": \"" + r.minKling + "\"}'\nexit 0\n",
		"kling-bridge-" + plat: "#!/bin/sh\nexit 0\n",
	}
	if plat == "darwin-arm64" {
		binarios["kling-vz-darwin-arm64"] = "#!/bin/sh\nexit 0\n"
		if !r.sinGuest {
			binarios["kling-guest-linux-arm64"] = "#!/bin/sh\nexit 0\n"
		}
	}
	if r.klingReal != "" {
		b, err := os.ReadFile(r.klingReal)
		if err != nil {
			t.Fatal(err)
		}
		binarios["kling-"+plat] = string(b)
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
	if r.firmaMinisig != nil {
		escribir(t, filepath.Join(dirTag, "SHA256SUMS.minisig"), string(r.firmaMinisig(firmado, tagRel)), 0o644)
	}
	if r.firmaSSH != nil {
		escribir(t, filepath.Join(dirTag, "SHA256SUMS.sshsig"), string(r.firmaSSH(firmado, tagRel)), 0o644)
	}
	if !r.sinFirma && r.firmaMinisig == nil {
		tag := r.tagFirmado
		if tag == "" {
			tag = tagRel
		}
		// Línea 1: el sha256 del SHA256SUMS firmado; línea 2: el comentario
		// de confianza con sus campos separados por tabuladores.
		escribir(t, filepath.Join(dirTag, "SHA256SUMS.minisig"),
			sha256Hex([]byte(firmado))+"\ntimestamp:1\tfile:SHA256SUMS\ttag:"+tag+"\n", 0o644)
	}

	// curl falso: -o DESTINO URL, sirve releases/download/<tag>/<nombre>.
	// Se niega a bajar nada sin --proto y --proto-redir '=https': install.sh
	// no puede caer a http ni seguir una redirección a otro protocolo.
	sistema := "/usr/bin:/bin"
	if r.conWget {
		// wget falso con la misma regla: sin --https-only no baja nada.
		escribir(t, filepath.Join(falsos, "wget"), `#!/bin/sh
dest=""; url=""; https=""
while [ $# -gt 0 ]; do
    case "$1" in
        --help) echo "  --https-only   only follow secure HTTPS links"; exit 0 ;;
        -O) dest="$2"; shift 2 ;;
        --https-only) https=1; shift ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
[ "$https" = 1 ] || { echo "wget sin --https-only" >&2; exit 98; }
f="`+datos+`/${url##*/releases/download/}"
[ -f "$f" ] || exit 8
cp "$f" "$dest"
`, 0o755)
		sistema = sinDescargadores(t, filepath.Join(raiz, "sistema"))
	} else {
		escribir(t, filepath.Join(falsos, "curl"), `#!/bin/sh
dest=""; url=""; proto=""; redir=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) dest="$2"; shift 2 ;;
        --proto) proto="$2"; shift 2 ;;
        --proto-redir) redir="$2"; shift 2 ;;
        -w|--retry) shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
[ "$proto" = "=https" ] && [ "$redir" = "=https" ] || { echo "curl sin --proto/--proto-redir =https" >&2; exit 98; }
case "$url" in
    */releases/latest) [ "$dest" = /dev/null ] || exit 2; echo "${url%/latest}/tag/`+tagRel+`"; exit 0 ;;
esac
f="`+datos+`/${url##*/releases/download/}"
[ -f "$f" ] || exit 22
cp "$f" "$dest"
`, 0o755)
	}
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
	if r.plat != "" {
		so, arq, _ := strings.Cut(r.plat, "-")
		escribir(t, filepath.Join(falsos, "uname"), `#!/bin/sh
case "$1" in
    -s) echo `+strings.ToUpper(so[:1])+so[1:]+` ;;
    -m) echo `+arq+` ;;
    *) echo `+strings.ToUpper(so[:1])+so[1:]+` ;;
esac
`, 0o755)
	}
	if r.klingPATH != "" {
		escribir(t, filepath.Join(falsos, "kling"), "#!/bin/sh\necho 'kling "+r.klingPATH+"'\n", 0o755)
	}

	res := resultadoInstall{
		prefijo: filepath.Join(home, ".local", "bin"),
		plugins: filepath.Join(home, "plugins"),
	}
	todos := []string{filepath.Join("install.sh")}
	if !r.latest {
		todos = append(todos, "--tag", tagRel)
	}
	todos = append(append(todos, "--prefix", res.prefijo, "--plugin-dir", res.plugins, "--no-rc"), args...)
	cmd := exec.Command("/bin/sh", todos...)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + falsos + ":" + sistema, "LC_ALL=C"}, r.env...)
	out, err := cmd.CombinedOutput()
	res.salida = string(out)
	res.ok = err == nil
	return res
}

// sinDescargadores llena dir de enlaces a lo de /usr/bin y /bin salvo curl y
// wget, para un PATH donde install.sh no encuentre el curl del sistema.
func sinDescargadores(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"/usr/bin", "/bin"} {
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			n := e.Name()
			// Lstat y no existe(): en Ubuntu /bin es /usr/bin, y un enlace ya
			// puesto a un ejecutable que es a su vez un enlace roto
			// (grub-ntldr-img) no «existe» por Stat pero ocupa el nombre.
			if _, err := os.Lstat(filepath.Join(dir, n)); n == "curl" || n == "wget" || err == nil {
				continue
			}
			if err := os.Symlink(filepath.Join(d, n), filepath.Join(dir, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// Sin curl, install.sh baja con wget y solo con --https-only: una redirección
// no puede llevar la descarga a http.
func TestInstallWgetSoloHTTPS(t *testing.T) {
	r := correrInstall(t, releaseFalsa{conWget: true})
	if !r.ok || !existe(filepath.Join(r.prefijo, "kling")) {
		t.Fatalf("con wget no instaló:\n%s", r.salida)
	}
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
		"firma de otra etiqueta": {releaseFalsa{tagFirmado: "v0.9.0"}, "is not signed for " + tagInstall},
		"sin SHA256SUMS.minisig": {releaseFalsa{sinFirma: true}, "has no SHA256SUMS.minisig"},
		"SHA256SUMS alterado":    {releaseFalsa{sumaAlterada: true}, "the SHA256SUMS signature is not valid"},
		"binario alterado":       {releaseFalsa{binAlterado: true}, "does not match"},
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

// Una 0.x sin firma solo se instala pidiéndola (--tag o KLING_VERSION), con
// aviso: como «latest» se rechaza, con o sin minisign, y el error da la orden.
// Una 1.x sin firma se rechaza siempre.
func TestInstallLatestCeroXSinFirmaFalla(t *testing.T) {
	casos := []struct {
		nombre string
		rel    releaseFalsa
		ok     bool
		motivo string
	}{
		{"0.x pedida con --tag", releaseFalsa{tag: "v0.20.1", sinFirma: true}, true, "checking with sha256 only"},
		{"0.x sin firma como latest", releaseFalsa{tag: "v0.20.1", sinFirma: true, latest: true}, false, "KLING_VERSION=v0.20.1"},
		{"0.x sin firma como latest, sin minisign", releaseFalsa{tag: "v0.20.1", sinFirma: true, latest: true, sinMinisign: true}, false, "KLING_VERSION=v0.20.1"},
		{"0.x firmada como latest", releaseFalsa{tag: "v0.20.1", latest: true}, true, "SHA256SUMS signature"},
		{"1.x firmada como latest", releaseFalsa{latest: true}, true, "SHA256SUMS signature"},
		{"1.x sin firma como latest", releaseFalsa{sinFirma: true, latest: true}, false, "has no SHA256SUMS.minisig"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			r := correrInstall(t, c.rel)
			instalado := existe(filepath.Join(r.prefijo, "kling"))
			if r.ok != c.ok || instalado != c.ok {
				t.Fatalf("salió ok=%v, instalado=%v; esperaba %v:\n%s", r.ok, instalado, c.ok, r.salida)
			}
			if !strings.Contains(r.salida, c.motivo) {
				t.Fatalf("la salida no dice %q:\n%s", c.motivo, r.salida)
			}
		})
	}
}

// Con todo dentro de main, un script cortado a medias (una descarga que se
// corta) no ejecuta nada: ni imprime los pasos ni crea el HOME. La última
// línea se corta además en cada byte: cortada tras `main`, sin los
// argumentos, tampoco puede correr (instalaría sin --tag ni --prefix).
func TestInstallCortadoNoEjecutaNada(t *testing.T) {
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimRight(string(b), "\n")
	fin := strings.LastIndex(s, "\n")
	if fin < 0 || !strings.Contains(s[fin:], `main "$@"`) {
		t.Fatal("install.sh no acaba llamando a main")
	}
	cortes := []int{len(b) / 4, len(b) / 2, len(b) * 3 / 4}
	for c := fin + 1; c < len(s); c++ {
		cortes = append(cortes, c)
	}
	// Un curl que falla: si algo llegara a correr, no sale a la red.
	falsos := t.TempDir()
	escribir(t, filepath.Join(falsos, "curl"), "#!/bin/sh\nexit 7\n", 0o755)
	for _, corte := range cortes {
		t.Run(fmt.Sprint(corte), func(t *testing.T) {
			raiz := t.TempDir()
			cortado := filepath.Join(raiz, "install.sh")
			c := corte
			if c <= fin {
				// Corta en un salto de línea, como una descarga parada entre paquetes.
				c = strings.LastIndex(string(b[:corte]), "\n") + 1
			}
			escribir(t, cortado, string(b[:c]), 0o755)
			home := filepath.Join(raiz, "home")
			cmd := exec.Command("/bin/sh", cortado, "--dry-run", "--tag", tagInstall)
			cmd.Env = []string{"HOME=" + home, "PATH=" + falsos + ":/usr/bin:/bin", "LC_ALL=C"}
			out, _ := cmd.CombinedOutput()
			if strings.Contains(string(out), "platform:") || strings.Contains(string(out), "(dry-run)") ||
				(c > fin && len(out) > 0 && !strings.Contains(strings.ToLower(string(out)), "syntax error")) || existe(home) {
				t.Fatalf("cortado en el byte %d, el script hizo algo:\n%s", c, out)
			}
		})
	}
}

// klingFalso es el kling de la release falsa: dice su versión y sabe lo que el
// kling de verdad hace con `up -verify-sums` (lo que install.sh usa cuando no
// hay minisign): el sha256 del fichero tiene que ser la línea 1 de la firma, y
// su comentario de confianza, el tag pedido.
func klingFalso(tag string) string {
	return `#!/bin/sh
if [ "$1" = up ] && [ "$2" = -verify-sums ]; then
    m="$3"; shift 3; x=""; t=""
    while [ $# -gt 0 ]; do
        case "$1" in
            -sig) x="$2"; shift 2 ;;
            -tag) t="$2"; shift 2 ;;
            -key) shift 2 ;;
            *) shift ;;
        esac
    done
    if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$m" | awk '{print $1}')"
    else got="$(shasum -a 256 "$m" | awk '{print $1}')"; fi
    [ "$(sed -n 1p "$x")" = "$got" ] || { echo "signature does not match: the file was altered" >&2; exit 1; }
    sed -n 2p "$x" | tr '\t' '\n' | grep -qx "tag:$t" || { echo "signed for another release: not signed for $t" >&2; exit 1; }
    echo "signature ok for $t"
    exit 0
fi
echo 'kling ` + tag + `'
`
}

// D2: desde 1.0 la firma se comprueba con el kling recién bajado, sin que el
// usuario tenga minisign.
func TestInstallFirmadaSinMinisignEnElPATH(t *testing.T) {
	r := correrInstall(t, releaseFalsa{sinMinisign: true, latest: true})
	if !r.ok || !existe(filepath.Join(r.prefijo, "kling")) {
		t.Fatalf("a signed 1.x release must install without minisign:\n%s", r.salida)
	}
	if strings.Contains(r.salida, "brew install minisign") {
		t.Fatalf("it must not send the user to install minisign:\n%s", r.salida)
	}
	if !strings.Contains(r.salida, "sha256 + self-check") || strings.Contains(r.salida, "SHA256SUMS signature") {
		t.Fatalf("it should say sha256 + self-check, not that a signature was checked:\n%s", r.salida)
	}
}

func TestInstallSinMinisignRechazaFirmaMala(t *testing.T) {
	for nombre, rel := range map[string]releaseFalsa{
		"sin firma":           {sinMinisign: true, sinFirma: true},
		"firma de otra etiq.": {sinMinisign: true, tagFirmado: "v0.9.0"},
	} {
		t.Run(nombre, func(t *testing.T) {
			r := correrInstall(t, rel)
			if r.ok || existe(filepath.Join(r.prefijo, "kling")) {
				t.Fatalf("installed with a bad signature:\n%s", r.salida)
			}
		})
	}
}

// D3: en Mac, install.sh deja el agente de invitado donde el daemon lo busca.
func TestInstallMacDejaLibKlingGuest(t *testing.T) {
	r := correrInstall(t, releaseFalsa{plat: "darwin-arm64"})
	if !r.ok {
		t.Fatalf("install failed:\n%s", r.salida)
	}
	home := filepath.Dir(r.plugins)
	g := filepath.Join(home, "Library", "Application Support", "kindling", "lib", "kling-guest")
	if !existe(g) {
		t.Fatalf("lib/kling-guest missing at %s:\n%s", g, r.salida)
	}
}

func TestInstallMacSinGuestEnLaReleaseSigue(t *testing.T) {
	r := correrInstall(t, releaseFalsa{plat: "darwin-arm64", sinGuest: true})
	if !r.ok {
		t.Fatalf("an older release without kling-guest must still install:\n%s", r.salida)
	}
}

// D1/D35: el final recomienda probar, no doctor, y la salida es inglesa.
func TestInstallSalidaEnInglesYRecomiendaTry(t *testing.T) {
	r := correrInstall(t, releaseFalsa{})
	if !r.ok {
		t.Fatalf("install failed:\n%s", r.salida)
	}
	if !strings.Contains(r.salida, "kling try -- uname -a") {
		t.Fatalf("no kling try suggestion:\n%s", r.salida)
	}
	if strings.Contains(r.salida, "kling doctor") {
		t.Fatalf("it should recommend kling try, not doctor:\n%s", r.salida)
	}
	for _, es := range []string{"descargando", "verificado", "instalado en", "plataforma", "Comprobar", "listo"} {
		if strings.Contains(r.salida, es) {
			t.Fatalf("Spanish %q in the output:\n%s", es, r.salida)
		}
	}
}

// claveSSH genera una clave ed25519 de usar y tirar y devuelve el allowed_signers
// que la nombra y una función que firma «tag:<etiqueta>\n<sums>» con ella.
func claveSSH(t *testing.T) (signers string, firmar func(sums, tag string) []byte) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen cannot make a key: %v %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	campos := strings.Fields(string(pub))
	signers = "kling-release " + campos[0] + " " + campos[1]
	n := 0
	return signers, func(sums, tag string) []byte {
		n++
		msg := filepath.Join(dir, fmt.Sprintf("msg%d", n))
		escribir(t, msg, "tag:"+tag+"\n"+sums, 0o644)
		if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-q", "-f", key, "-n", "kling-release", msg).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen -Y sign: %v %s", err, out)
		}
		b, err := os.ReadFile(msg + ".sig")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

// R20: sin minisign, una release con SHA256SUMS.sshsig se comprueba con
// ssh-keygen (independiente del binario que se instala); una firma de otra
// etiqueta o de otra clave no vale.
func TestInstallSinMinisignUsaSSHKeygen(t *testing.T) {
	signers, firmar := claveSSH(t)
	_, otraFirma := claveSSH(t)
	casos := []struct {
		nombre string
		rel    releaseFalsa
		ok     bool
		motivo string
	}{
		{"firma buena", releaseFalsa{firmaSSH: firmar}, true, "SHA256SUMS signature (ssh-keygen)"},
		{"firma de otra etiqueta", releaseFalsa{firmaSSH: func(s, _ string) []byte { return firmar(s, "v0.9.0") }}, false, "not valid"},
		{"firma de otra clave", releaseFalsa{firmaSSH: otraFirma}, false, "not valid"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			c.rel.sinMinisign = true
			c.rel.sinFirma = true
			c.rel.env = []string{"KLING_RELEASE_SSH_SIGNERS=" + signers}
			r := correrInstall(t, c.rel)
			if r.ok != c.ok || existe(filepath.Join(r.prefijo, "kling")) != c.ok {
				t.Fatalf("ok=%v, esperaba %v:\n%s", r.ok, c.ok, r.salida)
			}
			if !strings.Contains(r.salida, c.motivo) {
				t.Fatalf("la salida no dice %q:\n%s", c.motivo, r.salida)
			}
		})
	}
}

// Un test une install.sh con el kling de verdad (no el falso en shell): lo
// compila, firma SHA256SUMS con una clave de pruebas y comprueba que el
// verificador de Go acepta la buena, rechaza la de otra etiqueta y que el
// mensaje no la hace pasar por una firma independiente.
func TestInstallConKlingRealSinMinisign(t *testing.T) {
	if testing.Short() {
		t.Skip("compila kling")
	}
	bin := filepath.Join(t.TempDir(), "kling")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/kling").CombinedOutput(); err != nil {
		t.Skipf("cannot build kling here: %v\n%s", err, out)
	}
	k, err := upgrade.GenerarClave(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := k.Publica().String()
	firmar := func(otraTag string) func(string, string) []byte {
		return func(sums, tag string) []byte {
			if otraTag != "" {
				tag = otraTag
			}
			b, err := upgrade.Firmar(k, []byte(sums), "timestamp:1\tfile:SHA256SUMS\ttag:"+tag)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	buena := correrInstall(t, releaseFalsa{sinMinisign: true, klingReal: bin, firmaMinisig: firmar(""), env: []string{"KLING_RELEASE_PUBKEYS=" + pub}})
	if !buena.ok || !strings.Contains(buena.salida, "sha256 + self-check") {
		t.Fatalf("a release signed with the test key must install through the real kling:\n%s", buena.salida)
	}
	mala := correrInstall(t, releaseFalsa{sinMinisign: true, klingReal: bin, firmaMinisig: firmar("v0.9.0"), env: []string{"KLING_RELEASE_PUBKEYS=" + pub}})
	if mala.ok || existe(filepath.Join(mala.prefijo, "kling")) {
		t.Fatalf("a signature for another tag must be refused by the real kling:\n%s", mala.salida)
	}
}
