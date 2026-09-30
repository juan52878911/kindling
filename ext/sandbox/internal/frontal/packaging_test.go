package frontal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// La unidad publicada del frontal no lleva un usuario de nadie grabado: lleva
// @KLING_USER@/@KLING_GROUP@ y make deploy los rellena con el usuario al que
// el daemon cede su socket.
func TestUnidadSinUsuarioGrabado(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "kling-sandbox.service"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^(User|Group)=(.*)$`).FindAllStringSubmatch(string(b), -1)
	if len(m) != 2 || m[0][2] != "@KLING_USER@" || m[1][2] != "@KLING_GROUP@" {
		t.Errorf("User/Group = %v, want the @KLING_USER@/@KLING_GROUP@ placeholders", m)
	}
	mk, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), `s/@KLING_USER@/$$KU/`) || regexp.MustCompile(`ssh://juan@`).Match(mk) {
		t.Error("make deploy doesn't fill @KLING_USER@, or names a personal user")
	}
}
