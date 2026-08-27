package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func vivo(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Matar el pid del hijo NO mata a sus descendientes.
//
// Un servidor MCP lanzado a traves de un envoltorio —`npx`, un script— deja el
// proceso de verdad como NIETO. Por cada sesion cerrada quedaba uno vivo,
// reteniendo la RAM de su invitado: memoria que deriveMaxSessions no cuenta al
// decidir cuantas sesiones caben, asi que la microVM se quedaba sin memoria
// rotando sesiones.
func TestMatarGrupoSeLlevaTambienAlNieto(t *testing.T) {
	dir := t.TempDir()
	marca := filepath.Join(dir, "nieto.pid")

	// El hijo (sh) lanza un nieto (sleep) y espera. Es la forma exacta de un
	// envoltorio: quien trabaja de verdad es el nieto.
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! > "+marca+"; wait")
	enSuPropioGrupo(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("no arrancó: %v", err)
	}

	var nieto int
	limite := time.Now().Add(3 * time.Second)
	for time.Now().Before(limite) {
		if b, err := os.ReadFile(marca); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				nieto = p
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if nieto == 0 {
		t.Skip("el nieto no llegó a anunciarse en este entorno")
	}
	if !vivo(nieto) {
		t.Fatalf("el nieto %d ya no estaba vivo antes de matar nada", nieto)
	}

	matarGrupo(cmd)
	_ = cmd.Wait()

	// El nieto tiene que morir con el grupo.
	muerto := false
	limite = time.Now().Add(3 * time.Second)
	for time.Now().Before(limite) {
		if !vivo(nieto) {
			muerto = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !muerto {
		_ = syscall.Kill(nieto, syscall.SIGKILL) // no dejar basura en la máquina de quien corre los tests
		t.Errorf("el nieto %d sobrevivió: seguiría reteniendo RAM que nadie contabiliza", nieto)
	}
}

// enSuPropioGrupo tiene que ser lo que hace posible lo anterior: sin el, el
// hijo comparte grupo con el proceso de tests, y matar "-pid" seria matarnos.
func TestEnSuPropioGrupoLoSeparaDelProcesoQueLoLanza(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 5")
	enSuPropioGrupo(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("no arrancó: %v", err)
	}
	defer func() { matarGrupo(cmd); _ = cmd.Wait() }()

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("pgid=%d, pid=%d: el hijo no es líder de su grupo, y matar -pid alcanzaría a quien lo lanzó",
			pgid, cmd.Process.Pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Error("el hijo comparte grupo con el proceso de tests")
	}
}
