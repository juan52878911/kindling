package guest

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestInitBinfmtRegistra(t *testing.T) {
	f := newFakeInit()
	f.files[api.GuestEmulateFile] = "amd64\n"
	f.files[api.GuestEmulators["amd64"]] = ""
	initBinfmt(f)
	want := []string{
		"mount binfmt_misc /proc/sys/fs/binfmt_misc binfmt_misc 0xe",
		"write /proc/sys/fs/binfmt_misc/register",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	// La de Debian (qemu-x86_64.conf) con nuestro intérprete y banderas.
	line := `:qemu-x86_64:M::\x7f\x45\x4c\x46\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:` +
		`\xff\xff\xff\xff\xff\xfe\xfe\xfc\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:` +
		`/usr/local/lib/kindling/qemu-x86_64-binfmt-P:POCF`
	if got := f.files[binfmtRegister]; got != line {
		t.Fatalf("register:\n%s\nwant:\n%s", got, line)
	}
}

func TestInitBinfmtNada(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
	}{
		{"native image", nil},
		{"unknown arch", map[string]string{api.GuestEmulateFile: "riscv64\n"}},
		{"no interpreter", map[string]string{api.GuestEmulateFile: "amd64\n"}},
		{"already registered", map[string]string{api.GuestEmulateFile: "amd64", api.GuestEmulators["amd64"]: "",
			binfmtRegister: "", binfmtDir + "/qemu-x86_64": ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeInit()
			for k, v := range c.files {
				f.files[k] = v
			}
			initBinfmt(f)
			if len(f.calls) != 0 {
				t.Fatalf("calls: %q", f.calls)
			}
		})
	}
}

// Ya montado (otro init lo hizo): solo se registra.
func TestInitBinfmtMontado(t *testing.T) {
	f := newFakeInit()
	f.files[api.GuestEmulateFile] = "amd64"
	f.files[api.GuestEmulators["amd64"]] = ""
	f.files[binfmtRegister] = ""
	initBinfmt(f)
	if strings.Join(f.calls, ",") != "write "+binfmtRegister {
		t.Fatalf("calls: %q", f.calls)
	}
}
