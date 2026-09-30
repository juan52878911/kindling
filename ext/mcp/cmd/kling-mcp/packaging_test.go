package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Las unidades publicadas (release y make deploy) no llevan un usuario de
// nadie grabado: llevan @KLING_USER@/@KLING_GROUP@ y el deploy los rellena con
// el usuario al que el daemon cede su socket.
func TestUnidadesSinUsuarioGrabado(t *testing.T) {
	for _, u := range []string{"kling-gateway.service", "kling-heal.service"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "packaging", u))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if m := regexp.MustCompile(`(?m)^(User|Group)=(.*)$`).FindAllStringSubmatch(s, -1); len(m) != 2 ||
			m[0][2] != "@KLING_USER@" || m[1][2] != "@KLING_GROUP@" {
			t.Errorf("%s: User/Group = %v, want the @KLING_USER@/@KLING_GROUP@ placeholders", u, m)
		}
	}
	mk, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), `s/@KLING_USER@/$$KU/`) || !strings.Contains(string(mk), "KLING_SOCKET_USER") {
		t.Error("make deploy doesn't fill @KLING_USER@ from KLING_SOCKET_USER")
	}
	for _, dir := range []string{"packaging", "scripts"} {
		filepath.WalkDir(filepath.Join("..", "..", dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := os.ReadFile(p)
			if regexp.MustCompile(`\bjuan\b|/home/juan|ssh://juan@`).Match(b) {
				t.Errorf("%s names a personal user", p)
			}
			return nil
		})
	}
}
