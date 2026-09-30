package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func leerScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "80-mcp-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// -bundle corre esbuild como root en el chroot del host: ni `npx --yes` (sin
// versión y con los scripts de instalación activos), ni ENTRY metido en un
// `sh -c` entre comillas simples (una ruta con ' inyectaba órdenes).
func TestBundleEsbuildFijadoYSinShell(t *testing.T) {
	s := leerScript(t)
	if strings.Contains(s, "npx --yes esbuild") || strings.Contains(s, "npx -y esbuild") {
		t.Error("80-mcp-image.sh still fetches esbuild with npx (unpinned, install scripts on)")
	}
	if strings.Contains(s, `'$ENTRY'`) {
		t.Error("80-mcp-image.sh still interpolates ENTRY into shell text")
	}
	if !regexp.MustCompile(`(?m)^ESBUILD_VERSION=\d+\.\d+\.\d+$`).MatchString(s) {
		t.Error("esbuild has no pinned version")
	}
	for _, v := range []string{"ESBUILD_SHA512_LINUX_X64", "ESBUILD_SHA512_LINUX_ARM64"} {
		if !regexp.MustCompile(`(?m)^` + v + `="sha512-[A-Za-z0-9+/]{86}=="$`).MatchString(s) {
			t.Errorf("%s is not a pinned sha512", v)
		}
	}
	if !strings.Contains(s, `chroot "$mnt" /tmp/kling-esbuild/esbuild "$ENTRY"`) {
		t.Error("esbuild is not run directly with ENTRY as an argument")
	}
}

// valid_entry, la función del script, rechaza lo que la auditoría usó para
// inyectar (una ruta con ') y lo que esbuild tomaría por un flag.
func TestValidEntryDelScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	s := leerScript(t)
	i := strings.Index(s, "valid_entry() {")
	if i < 0 {
		t.Fatal("80-mcp-image.sh has no valid_entry()")
	}
	j := strings.Index(s[i:], "\n}\n")
	fn := s[i : i+j+3]
	for entry, ok := range map[string]bool{
		"/usr/local/lib/node_modules/@scope/pkg/dist/index.js": true,
		"/opt/srv/main.mjs": true,
		"/usr/lib/node_modules/d';echo INYECTADO>&2;'x/e.js": false,
		`/x/$(id).js`:           false,
		"/x/a b.js":             false,
		"--outfile=/etc/passwd": false,
		"relativo/index.js":     false,
		"":                      false,
	} {
		cmd := exec.Command(bash, "-c", fn+`valid_entry "$1"`, "bash", entry)
		out, err := cmd.CombinedOutput()
		if got := err == nil; got != ok {
			t.Errorf("valid_entry(%q) = %v, want %v (%s)", entry, got, ok, out)
		}
		if strings.Contains(string(out), "INYECTADO") {
			t.Errorf("valid_entry(%q) ran the injected command", entry)
		}
	}
}

// ValidateBuild aplica la misma lista antes de que la petición llegue al script.
func TestValidateBuildEntryDeBundle(t *testing.T) {
	base := BuildRequest{Name: "srv", NPM: []string{"pkg"}, Bundle: true}
	mala := base
	mala.Cmd = []string{"node", "/usr/lib/node_modules/d';echo INYECTADO>&2;'x/e.js"}
	if err := ValidateBuild(mala); err == nil {
		t.Error("ValidateBuild accepted a bundle entry with a quote")
	}
	buena := base
	buena.Cmd = []string{"node", "/usr/local/lib/node_modules/pkg/dist/index.js", "--port", "1"}
	if err := ValidateBuild(buena); err != nil {
		t.Errorf("ValidateBuild rejected a clean entry: %v", err)
	}
}
