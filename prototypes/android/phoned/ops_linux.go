package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// androidOps es phoneOps sobre el Android de este supervisor.
type androidOps struct {
	s *supervisor

	envMu  sync.Mutex
	envPID int
	env    []string
}

func (o *androidOps) initPID() (int, error) {
	o.s.mu.Lock()
	pid := o.s.pid
	o.s.mu.Unlock()
	if pid <= 0 || comm(pid) != "init" {
		return 0, errNotRunning
	}
	return pid, nil
}

// envFor cachea el entorno de zygote por init (cambia si Android se relanza).
func (o *androidOps) envFor(pid int) []string {
	o.envMu.Lock()
	defer o.envMu.Unlock()
	if o.envPID == pid && o.env != nil {
		return o.env
	}
	env := zygoteEnv(pid)
	if procByComm("system_server") > 0 { // con zygote ya vivo, el entorno es el bueno
		o.envPID, o.env = pid, env
	}
	return env
}

func (o *androidOps) run(ctx context.Context, timeout time.Duration, stdin *bytes.Reader, script string, args ...string) ([]byte, error) {
	pid, err := o.initPID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if stdin == nil {
		return runIn(ctx, pid, o.envFor(pid), nil, script, args...)
	}
	return runIn(ctx, pid, o.envFor(pid), stdin, script, args...)
}

func (o *androidOps) Health(ctx context.Context) healthInfo {
	s := o.s
	s.mu.Lock()
	h := healthInfo{State: s.state, Restarts: s.restarts, Net: s.netMode, Version: version, Kernel: kernelRelease()}
	pid, started := s.pid, s.started
	s.mu.Unlock()
	h.Verity = verityStatus()
	if pid > 0 && comm(pid) == "init" {
		h.AndroidPID = pid
		h.UptimeS = int64(time.Since(started).Seconds())
		v, err := getProp(pid, "sys.boot_completed")
		h.BootCompleted = err == nil && v == "1"
		sec, _ := getProp(pid, "ro.adb.secure")
		h.AdbSecure = sec == "1"
		h.SystemServer = procByComm("system_server") > 0
		h.Uidump = uidumpInstalled(pid)
	} else {
		h.Detail = "android is not running"
	}
	h.OK = h.AndroidPID > 0 && h.BootCompleted && h.SystemServer &&
		(h.Verity == "none" || h.Verity == "verified")
	if h.Verity != "none" && h.Verity != "verified" {
		h.Detail = "dm-verity: " + h.Verity
	}
	return h
}

func (o *androidOps) Screen(ctx context.Context) ([]byte, error) {
	out, err := o.run(ctx, 20*time.Second, nil, "exec screencap -p")
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(out, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, fmt.Errorf("screencap did not return a PNG (%d bytes)", len(out))
	}
	return out, nil
}

func (o *androidOps) Tree(ctx context.Context, compressed bool) ([]byte, string, error) {
	pid, err := o.initPID()
	if err != nil {
		return nil, "", err
	}
	line := "dump"
	if compressed {
		line = "dump --compressed"
	}
	if uidumpInstalled(pid) {
		if out, err := o.uidump(ctx, pid, line); err == nil {
			return out, "uidump", nil
		}
	}
	flag := ""
	if compressed {
		flag = "--compressed"
	}
	// uiautomator deja el XML en un fichero; se lee y se borra en la misma shell.
	out, err := o.run(ctx, 30*time.Second, nil,
		`f=/data/local/tmp/phoned-ui.$$.xml; uiautomator dump $1 "$f" >/dev/null 2>&1 && cat "$f"; rc=$?; rm -f "$f"; exit $rc`, flag)
	if err != nil {
		return nil, "", fmt.Errorf("uiautomator dump: %w", err)
	}
	return out, "uiautomator", nil
}

// uidump llama al servidor y, si no contesta, lo arranca por init
// (ctl.start kindling_uidump) y espera hasta 8 s, como el cliente de bash.
func (o *androidOps) uidump(ctx context.Context, pid int, line string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := uidumpCall(cctx, pid, line)
	if err == nil && len(out) > 0 {
		return out, nil
	}
	if _, serr := o.run(ctx, 5*time.Second, nil, "setprop ctl.start kindling_uidump"); serr != nil {
		return nil, serr
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if out, err = uidumpCall(cctx, pid, "ping"); err == nil && bytes.HasPrefix(out, []byte("ok")) {
			return uidumpCall(cctx, pid, line)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("uidump did not start: %v", err)
}

func (o *androidOps) Input(ctx context.Context, in inputReq) (string, error) {
	pid, err := o.initPID()
	if err != nil {
		return "", err
	}
	var line string
	switch in.Kind {
	case "tap":
		line = fmt.Sprintf("tap %d %d", in.X, in.Y)
	case "swipe":
		line = fmt.Sprintf("swipe %d %d %d %d %d", in.X, in.Y, in.X2, in.Y2, in.MS)
	case "text":
		line = "text " + in.Text
	case "key":
		line = "key " + in.Key
	}
	if uidumpInstalled(pid) {
		if _, err := o.uidump(ctx, pid, line); err == nil {
			return "uidump", nil
		}
	}
	// `input`: app_process, ~0,5 s por gesto. Argumentos ya validados, y aun
	// así como $1.. y no pegados al script.
	var script string
	var args []string
	switch in.Kind {
	case "tap":
		script, args = `exec input tap "$1" "$2"`, []string{strconv.Itoa(in.X), strconv.Itoa(in.Y)}
	case "swipe":
		script, args = `exec input swipe "$1" "$2" "$3" "$4" "$5"`,
			[]string{strconv.Itoa(in.X), strconv.Itoa(in.Y), strconv.Itoa(in.X2), strconv.Itoa(in.Y2), strconv.Itoa(in.MS)}
	case "text":
		// input text no admite espacios tal cual: %s es un espacio.
		script, args = `exec input text "$1"`, []string{strings.ReplaceAll(in.Text, " ", "%s")}
	case "key":
		script, args = `exec input keyevent "$1"`, []string{in.Key}
	}
	if _, err := o.run(ctx, 20*time.Second, nil, script, args...); err != nil {
		return "", err
	}
	return "input", nil
}

func (o *androidOps) Install(ctx context.Context, apk []byte) (string, error) {
	// pm por la entrada estándar (-S tamaño): nada de ficheros temporales ni
	// en la VM ni en Android.
	out, err := o.run(ctx, 4*time.Minute, bytes.NewReader(apk),
		`exec cmd package install -r -t -S "$1"`, strconv.Itoa(len(apk)))
	res := strings.TrimSpace(string(out))
	if err != nil {
		return res, err
	}
	if !strings.Contains(res, "Success") {
		return res, errors.New("install failed")
	}
	return res, nil
}

func (o *androidOps) Launch(ctx context.Context, pkg string) (string, error) {
	out, err := o.run(ctx, 30*time.Second, nil, `
		c=$(cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.LAUNCHER "$1" | tail -n 1)
		case "$c" in */*) exec am start -W -n "$c" ;; esac
		echo "no launcher activity for $1" >&2; exit 1`, pkg)
	return strings.TrimSpace(string(out)), err
}

func (o *androidOps) Logs(ctx context.Context, buffer string, lines int) ([]byte, error) {
	if buffer == "phoned" {
		return o.s.logs.tail(lines), nil
	}
	b := buffer
	if b == "all" {
		b = "main,system,crash,events"
	}
	return o.run(ctx, 20*time.Second, nil, `exec logcat -d -b "$1" -t "$2"`, b, strconv.Itoa(lines))
}

func (o *androidOps) Identity(ctx context.Context) (identityInfo, error) {
	pid, err := o.initPID()
	if err != nil {
		return identityInfo{}, err
	}
	var id identityInfo
	id.Serial, _ = getProp(pid, "ro.serialno")
	sec, _ := getProp(pid, "ro.adb.secure")
	id.AdbSecure = sec == "1"
	out, err := o.run(ctx, 30*time.Second, nil, `
		settings get secure android_id
		settings get global device_name
		k=/data/misc/adb/adb_keys; if [ -f $k ]; then wc -l < $k; else echo 0; fi
		f=/data/system/users/0/settings_ssaid.xml
		[ -f "$f" ] && { abx2xml "$f" - 2>/dev/null || cat "$f"; }
		exit 0`)
	if err != nil {
		return id, err
	}
	parts := strings.SplitN(string(out), "\n", 4)
	if len(parts) < 3 {
		return id, fmt.Errorf("unexpected settings output")
	}
	id.AndroidID = strings.TrimSpace(parts[0])
	id.DeviceName = strings.TrimSpace(parts[1])
	id.AdbKeys, _ = strconv.Atoi(strings.TrimSpace(parts[2]))
	if len(parts) == 4 {
		id.SSAIDUserKey, id.SSAIDPackages = parseSSAID([]byte(parts[3]))
	}
	return id, nil
}

// ── CLI: la sonda de listo y el gancho tras restaurar ────────────────────────

// runReady es /etc/kindling/ready: 0 cuando sys.boot_completed=1, leído de la
// memoria de propiedades de Android (sin procesos: microsegundos).
func runReady() int {
	pid, err := findInitPID()
	if err != nil {
		st, _ := os.ReadFile(statePath)
		fmt.Printf("android not running (%s)\n", strings.TrimSpace(string(st)))
		return 1
	}
	v, err := getProp(pid, "sys.boot_completed")
	if err != nil || v != "1" {
		fmt.Printf("sys.boot_completed=%s\n", orQ(v))
		return 1
	}
	// Con ANDROID_PREP=1, listo es además "pantalla preparada" (lo escribe el
	// supervisor con el PID de este init).
	if cfg, err := loadConfig(); err == nil && cfg.Prep {
		b, _ := os.ReadFile(prepMark)
		if strings.TrimSpace(string(b)) != strconv.Itoa(pid) {
			fmt.Println("boot_completed, preparing the screen")
			return 1
		}
	}
	return 0
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
