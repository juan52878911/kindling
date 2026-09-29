package dbstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testID = "00000000000db001"

func TestRolePassword(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	if _, err := ReadRolePassword(testID, "agent"); err != ErrNoPassword {
		t.Fatalf("want ErrNoPassword, got %v", err)
	}
	if err := WriteRolePassword(testID, "agent", "s3cret"); err != nil {
		t.Fatal(err)
	}
	p, _ := RolePasswordPath(testID, "agent")
	if filepath.Base(p) != "agent.password" || filepath.Dir(p) != mustCopyDir(t) {
		t.Fatalf("path %s", p)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if pw, err := ReadRolePassword(testID, "agent"); err != nil || pw != "s3cret" {
		t.Fatalf("%q %v", pw, err)
	}
	// Distinto del de la aplicación.
	if _, err := ReadPassword(testID); err != ErrNoPassword {
		t.Fatalf("app password: %v", err)
	}
	// Rotar de nuevo pisa el fichero.
	if err := WriteRolePassword(testID, "agent", "other"); err != nil {
		t.Fatal(err)
	}
	if pw, _ := ReadRolePassword(testID, "agent"); pw != "other" {
		t.Fatalf("%q", pw)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRolePassword(testID, "agent"); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("%v", err)
	}
	os.Chmod(p, 0o600)
	if err := RemoveRolePassword(testID, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRolePassword(testID, "agent"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestRolePasswordRechaza(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	for _, r := range []string{"", "../x", "a/b", "A", "a.b", "password/../x"} {
		if _, err := RolePasswordPath(testID, r); err == nil {
			t.Errorf("role %q: want error", r)
		}
	}
	if err := WriteRolePassword(testID, "agent", "a\nb"); err == nil {
		t.Error("multi-line password accepted")
	}
	if err := WriteRolePassword("../x", "agent", "p"); err == nil {
		t.Error("bad id accepted")
	}
	// Un enlace no vale como fichero de contraseña.
	if err := WriteRolePassword(testID, "agent", "p"); err != nil {
		t.Fatal(err)
	}
	p, _ := RolePasswordPath(testID, "agent")
	target := filepath.Join(t.TempDir(), "t")
	os.WriteFile(target, []byte("x\n"), 0o600)
	os.Remove(p)
	os.Symlink(target, p)
	if _, err := ReadRolePassword(testID, "agent"); err == nil {
		t.Error("symlink accepted")
	}
	// Remove de la copia se lleva las claves de los roles.
	os.Remove(p)
	WriteRolePassword(testID, "agent", "p")
	if err := Remove(testID); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRolePassword(testID, "agent"); err != ErrNoPassword {
		t.Fatalf("%v", err)
	}
}

func mustCopyDir(t *testing.T) string {
	t.Helper()
	d, err := CopyDir(testID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
