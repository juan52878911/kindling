package main

// stage2: el hijo que acaba siendo init de Android. Corre ya en los espacios
// de nombres nuevos (los pidió el clone del padre) y es PID 1 del suyo.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func runStage2() error {
	if os.Getpid() != 1 {
		return errors.New("stage2 is internal: kling-phoned runs it as PID 1 of a new PID namespace")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// La orden del padre: argumentos extra de red y un NUL cuando la red está.
	ctl := os.NewFile(3, "ctl")
	msg, err := bufio.NewReader(ctl).ReadString(0)
	ctl.Close()
	if err != nil {
		return fmt.Errorf("parent went away before the go-ahead: %v", err)
	}
	var netArgs []string
	for _, l := range strings.Split(strings.TrimSuffix(msg, "\x00"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			netArgs = append(netArgs, l)
		}
	}
	return stage2(cfg, netArgs)
}

func mount(src, dst, fstype string, flags uintptr, data string) error {
	if err := syscall.Mount(src, dst, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s (%s): %w", src, dst, fstype, err)
	}
	return nil
}

func stage2(cfg config, netArgs []string) error {
	R := cfg.Root
	_ = syscall.Sethostname([]byte("localhost"))
	// Nada de lo que se monte aquí sale a la VM (lo que era --propagation private).
	if err := mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return err
	}
	// La raíz de Android tiene que ser un punto de montaje: init de AOSP hace
	// mount(NULL, "/", MS_REC|MS_SHARED) y con / como directorio aborta
	// (README, fallo 1).
	if err := mount(R, R, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return err
	}
	for _, d := range []string{"proc", "sys", "dev"} {
		_ = os.MkdirAll(filepath.Join(R, d), 0o755)
	}
	if err := mount("proc", R+"/proc", "proc", 0, ""); err != nil {
		return err
	}
	if err := mount("sysfs", R+"/sys", "sysfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return err
	}
	_ = os.MkdirAll(R+"/sys/fs/cgroup", 0o755)
	if mount("cgroup2", R+"/sys/fs/cgroup", "cgroup2", 0, "nsdelegate") != nil {
		if err := mount("cgroup2", R+"/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
			fmt.Fprintf(os.Stderr, "aviso: %v\n", err)
		}
	}
	// /dev: tmpfs propio con copia de los nodos del invitado. Montar el devtmpfs
	// de la VM tal cual dejaría a Android crear sus sockets en el /dev de
	// kling-guest.
	if err := mount("tmpfs", R+"/dev", "tmpfs", syscall.MS_NOSUID, "mode=0755,size=65536k"); err != nil {
		return err
	}
	if err := copyDev("/dev", R+"/dev"); err != nil {
		return fmt.Errorf("copy /dev: %w", err)
	}
	for _, d := range []string{"pts", "shm", "mqueue"} {
		_ = os.MkdirAll(filepath.Join(R, "dev", d), 0o755)
	}
	if err := mount("devpts", R+"/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil {
		return err
	}
	_ = os.Remove(R + "/dev/ptmx")
	_ = os.Symlink("pts/ptmx", R+"/dev/ptmx")
	if err := mount("tmpfs", R+"/dev/shm", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777"); err != nil {
		return err
	}
	_ = mount("mqueue", R+"/dev/mqueue", "mqueue", 0, "")

	// binderfs: el init.rc de AOSP 12+ lo monta él; si esta imagen no, aquí.
	rc, _ := os.ReadFile(R + "/system/etc/init/hw/init.rc")
	if !strings.Contains(string(rc), "mount binder binder /dev/binderfs") {
		_ = os.MkdirAll(R+"/dev/binderfs", 0o755)
		if err := mount("binder", R+"/dev/binderfs", "binder", 0, ""); err != nil {
			return err
		}
		for _, d := range []string{"binder", "hwbinder", "vndbinder"} {
			p := R + "/dev/binderfs/" + d
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("binderfs has no %s: CONFIG_ANDROID_BINDER_DEVICES must list it", d)
			}
			_ = os.Chmod(p, 0o666)
			_ = os.Symlink("binderfs/"+d, R+"/dev/"+d)
		}
	}

	// /data: en el overlay de la VM (por defecto) o en RAM.
	_ = os.MkdirAll(R+"/data", 0o771)
	if cfg.DataMode == "tmpfs" {
		ram := stateDir + "/data"
		_ = os.MkdirAll(ram, 0o700)
		if err := mount("tmpfs", ram, "tmpfs", 0, "mode=0700,size="+cfg.DataSize); err != nil {
			return err
		}
		if err := copySparse(dataImgPath, ram+"/data.ext4"); err != nil {
			return fmt.Errorf("data image: %w", err)
		}
		dev, lf, err := attachLoop(ram + "/data.ext4")
		if err != nil {
			return err
		}
		err = mount(dev, R+"/data", "ext4", syscall.MS_NOATIME, "discard")
		lf.Close() // tras montar: el montaje retiene el loop (autoborrado al desmontar)
		if err != nil {
			return err
		}
		_ = os.Chmod(R+"/data", 0o771)
		_ = os.Chown(R+"/data", 1000, 1000) // system:system, como en la imagen
	}

	args := initArgs(cfg, netArgs)
	fmt.Fprintf(os.Stderr, "kling-phoned: exec /init %s\n", strings.Join(args[1:], " "))

	// pivot_root y no chroot (README, fallo 3: con chroot zygote ve sus ficheros
	// como /android/apex/... y los rechaza). Binario estático: tras el pivot no
	// hace falta ningún cargador de la raíz vieja.
	const old = ".kindling-vm"
	if err := os.Chdir(R); err != nil {
		return err
	}
	_ = os.MkdirAll(old, 0o700)
	if err := syscall.PivotRoot(".", old); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := syscall.Unmount("/"+old, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("umount old root: %w", err)
	}
	_ = os.Remove("/" + old)
	env := []string{"PATH=/system/bin:/system/xbin", "HOSTNAME=localhost"}
	return syscall.Exec("/init", args, env)
}

// initArgs: los argumentos de la imagen (ENTRYPOINT sin argv[0]), los de
// siempre del lanzador y los de la red.
func initArgs(cfg config, netArgs []string) []string {
	args := []string{"/init"}
	if b, err := os.ReadFile(argsPath); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				args = append(args, l)
			}
		}
	}
	args = append(args,
		"androidboot.redroid_width="+cfg.Width,
		"androidboot.redroid_height="+cfg.Height,
		"androidboot.redroid_dpi="+cfg.DPI,
		"androidboot.redroid_fps="+cfg.FPS,
		"androidboot.redroid_gpu_mode=guest", // SwiftShader en CPU
		"androidboot.use_memfd=true",         // 6.1 no tiene ashmem
		"ro.setupwizard.mode=DISABLED",
		// La serie de fábrica: la de cada clon la pone identity.go en su sitio
		// (misma longitud máxima que cualquier valor: 91 bytes).
		"androidboot.serialno="+goldenSerial,
	)
	if cfg.AdbSecure {
		args = append(args, "ro.adb.secure=1")
	}
	args = append(args, cfg.ExtraArgs...)
	return append(args, netArgs...)
}

// goldenSerial es la serie con la que arranca Android en frío (el dorado).
const goldenSerial = "KINDLINGGOLDEN"

// copyDev copia el árbol de /dev sin cruzar a otros montajes (cp -ax):
// directorios, enlaces, nodos de dispositivo y FIFO con su modo y dueño.
func copyDev(src, dst string) error {
	var root syscall.Stat_t
	if err := syscall.Lstat(src, &root); err != nil {
		return err
	}
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // un nodo que desaparece mientras se copia
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if st.Dev != root.Dev {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		t := filepath.Join(dst, rel)
		mode := uint32(fi.Mode().Perm())
		switch {
		case fi.IsDir():
			if err := os.Mkdir(t, fi.Mode().Perm()); err != nil && !os.IsExist(err) {
				return err
			}
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			_ = os.Symlink(l, t)
			_ = os.Lchown(t, int(st.Uid), int(st.Gid))
			return nil
		case fi.Mode()&os.ModeDevice != 0, fi.Mode()&os.ModeNamedPipe != 0:
			if err := syscall.Mknod(t, st.Mode, int(st.Rdev)); err != nil {
				return nil
			}
		case fi.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return nil
			}
			out, err := os.OpenFile(t, os.O_CREATE|os.O_WRONLY, fi.Mode().Perm())
			if err == nil {
				_, _ = io.Copy(out, in)
				out.Close()
			}
			in.Close()
		default:
			return nil // sockets: son de quien los creó
		}
		_ = os.Lchown(t, int(st.Uid), int(st.Gid))
		_ = syscall.Chmod(t, mode|st.Mode&(syscall.S_ISUID|syscall.S_ISGID|syscall.S_ISVTX))
		return nil
	})
}
