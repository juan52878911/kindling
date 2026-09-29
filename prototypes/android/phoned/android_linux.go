package main

// Operar Android desde la VM: encontrar su init, leer y escribir propiedades
// en memoria, ejecutar herramientas de Android dentro de sus espacios de
// nombres y hablar con uidump.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// findInitPID: el hijo con comm "init" del PID de unshare.pid (lo escribe
// kling-phoned al arrancar; con el lanzador de bash era el de unshare). Es lo
// mismo que hacen android-sh y uidump, para que los tres vean al mismo init.
func findInitPID() (int, error) {
	b, err := os.ReadFile(parentPIDPth)
	if err != nil {
		return 0, errNotRunning
	}
	parent, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || parent <= 0 {
		return 0, errNotRunning
	}
	for _, c := range childrenOf(parent) {
		if comm(c) == "init" {
			return c, nil
		}
	}
	return 0, errNotRunning
}

func comm(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return strings.TrimSpace(string(b))
}

func ppid(pid int) int {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PPid:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return -1
}

// childrenOf recorre /proc (en Go, sin un proceso por PID como el bash viejo).
func childrenOf(parent int) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid(n) == parent {
			out = append(out, n)
		}
	}
	return out
}

// procByComm: el primer proceso de la VM con ese comm (system_server).
func procByComm(name string) int {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err == nil && comm(n) == name {
			return n
		}
	}
	return 0
}

// ── propiedades en memoria (props.go) ───────────────────────────────────────

func propDir(initPID int) string { return fmt.Sprintf("/proc/%d/root/dev/__properties__", initPID) }

// mapArea mapea un fichero de propiedades; write=true para reescribir.
func mapArea(path string, write bool) (*propArea, func(), error) {
	flag, prot := os.O_RDONLY, syscall.PROT_READ
	if write {
		flag, prot = os.O_RDWR, syscall.PROT_READ|syscall.PROT_WRITE
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if st.Size() < propHeaderSize || st.Size() > 16<<20 {
		return nil, nil, fmt.Errorf("%s: unexpected size %d", path, st.Size())
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), prot, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	a := &propArea{b: b, wake: futexWake}
	return a, func() { _ = syscall.Munmap(b) }, nil
}

func futexWake(addr *uint32) {
	const futexWakeOp = 1 // FUTEX_WAKE, sin _PRIVATE: la memoria es compartida
	_, _, _ = syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(addr)), futexWakeOp, math.MaxInt32, 0, 0, 0)
}

// withProp busca name en los ficheros de propiedades de Android y llama a fn
// con el área que lo tiene. El contexto de cada nombre lo dice property_info,
// pero recorrer los ~60 ficheros cuesta menos que interpretarlo.
func withProp(initPID int, name string, write bool, fn func(a *propArea) error) error {
	dir := propDir(initPID)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		n := e.Name()
		if n == "property_info" || n == "properties_serial" {
			continue
		}
		a, unmap, err := mapArea(filepath.Join(dir, n), write)
		if err != nil {
			continue
		}
		if _, err := a.find(name); err != nil {
			unmap()
			continue
		}
		err = fn(a)
		unmap()
		return err
	}
	return errPropNotFound
}

func getProp(initPID int, name string) (string, error) {
	var v string
	err := withProp(initPID, name, false, func(a *propArea) error {
		var e error
		v, e = a.get(name)
		return e
	})
	return v, err
}

// setPropInPlace reescribe una propiedad que ya existe, aunque sea ro.*.
func setPropInPlace(initPID int, name, value string) error {
	err := withProp(initPID, name, true, func(a *propArea) error { return a.set(name, value) })
	if err != nil {
		return err
	}
	a, unmap, err := mapArea(filepath.Join(propDir(initPID), "properties_serial"), true)
	if err != nil {
		return nil // el valor ya está; solo no se avisa a quien espera cambios
	}
	defer unmap()
	return a.bumpSerial()
}

// ── ejecutar dentro de Android ───────────────────────────────────────────────

// zygoteEnv: el entorno de zygote (BOOTCLASSPATH, ANDROID_ROOT...). Sin él
// uiautomator, am y pm, que arrancan con app_process, no funcionan. Antes de
// que exista zygote, uno mínimo que basta para getprop y settings.
func zygoteEnv(initPID int) []string {
	for _, c := range childrenOf(initPID) {
		cl, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", c))
		if bytes.HasPrefix(cl, []byte("zygote64")) || bytes.HasPrefix(cl, []byte("zygote\x00")) {
			env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", c))
			if err == nil && len(env) > 0 {
				var out []string
				for _, kv := range bytes.Split(env, []byte{0}) {
					if len(kv) > 0 {
						out = append(out, string(kv))
					}
				}
				return out
			}
		}
	}
	return []string{
		"PATH=/product/bin:/apex/com.android.runtime/bin:/apex/com.android.art/bin:/system_ext/bin:/system/bin:/system/xbin:/odm/bin:/vendor/bin:/vendor/xbin",
		"ANDROID_ROOT=/system", "ANDROID_DATA=/data", "ANDROID_STORAGE=/storage",
	}
}

// androidCmd prepara /system/bin/sh -c script dentro de todos los espacios de
// init de Android y en su raíz (nsenter de util-linux, en la base: Go no puede
// entrar en otro espacio de montajes desde un proceso con varios hilos), con
// el entorno de zygote. script es fijo de quien llama; lo variable va en args
// ($1, $2...) o por stdin, nunca pegado al script.
func androidCmd(ctx context.Context, initPID int, env []string, script string, args ...string) *exec.Cmd {
	argv := []string{"--target", strconv.Itoa(initPID), "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd",
		"--", "/system/bin/env", "-i"}
	argv = append(argv, env...)
	argv = append(argv, "HOME=/", "TMPDIR=/data/local/tmp", "/system/bin/sh", "-c", script, "sh")
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "nsenter", argv...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	// Grupo propio: al vencer el plazo muere también lo que nsenter lanzó
	// dentro del espacio de PIDs de Android.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// runIn ejecuta y devuelve stdout; el error lleva el final de stderr.
func runIn(ctx context.Context, initPID int, env []string, stdin io.Reader, script string, args ...string) ([]byte, error) {
	cmd := androidCmd(ctx, initPID, env, script, args...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		if ctx.Err() != nil {
			return out.Bytes(), fmt.Errorf("%w: %s", ctx.Err(), msg)
		}
		return out.Bytes(), fmt.Errorf("%v: %s", err, msg)
	}
	return out.Bytes(), nil
}

// ── uidump (prototypes/android/uidump) ───────────────────────────────────────

const uidumpSock = "/dev/socket/kindling-uidump"

// uidumpCall manda una línea al servidor residente y devuelve la respuesta
// entera (el servidor cierra al acabar). El socket se abre por la raíz de
// init (/proc/PID/root), sin entrar en sus espacios: no hace falta proceso.
func uidumpCall(ctx context.Context, initPID int, line string) ([]byte, error) {
	p := fmt.Sprintf("/proc/%d/root%s", initPID, uidumpSock)
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", p)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := io.WriteString(c, line+"\n"); err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(c, 64<<20))
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(out, []byte("ERROR:")) {
		return nil, errors.New(strings.TrimSpace(string(out)))
	}
	return out, nil
}

func uidumpInstalled(initPID int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d/root/system/etc/init/kindling-uidump.rc", initPID))
	return err == nil
}
