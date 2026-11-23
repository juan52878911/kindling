package main

// Primer uso: lo que hace falta entre `curl … | sh` y el primer `kling try`.
//
// kling try tiene que funcionar en una máquina recién instalada, sin pasos
// intermedios: si el daemon no corre lo arranca (por su agente de launchd en
// Mac, nunca como hijo suelto de este proceso), si no hay ninguna imagen baja
// la mínima de la release, y si no puede, falla con UNA orden que lo arregla.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/transport"
	"github.com/juan52878911/kindling/internal/upgrade"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// arrancarDaemonTry arranca el daemon local cuando try lo encuentra parado. Es
// una variable para sustituirla en los tests.
var arrancarDaemonTry = arrancarDaemonLocal

// bajarImagenesTry deja en la raíz del daemon local el kernel y min de la
// release de este kling. Variable por lo mismo.
var bajarImagenesTry = bajarImagenesLocal

// imagenesTry son las imágenes que try usa sin -image, por orden de
// preferencia: toolchain (node y python) y, si no está, min (lo mínimo).
var imagenesTry = []string{tryDefaultImage, "min"}

// raizLocal es la raíz de datos del daemon local.
func raizLocal() string {
	return config.Or(os.Getenv("KLING_ROOT"), transport.DefaultRoot())
}

// esRemoto dice si el endpoint no es un daemon de esta máquina.
func esRemoto(endpoint string) bool {
	return strings.Contains(endpoint, "://") && !strings.HasPrefix(endpoint, "unix://")
}

// asegurarDaemon comprueba que el daemon contesta y, si no corre (no hay
// quien escuche: cualquier respuesta, un 401 incluido, es que está), lo
// arranca. Un daemon remoto no es cosa de try.
func asegurarDaemon(ctx context.Context, c *api.Client, endpoint string) error {
	_, err := c.Info(ctx)
	if err == nil || esRemoto(endpoint) {
		return nil
	}
	var se *api.StatusError
	if errors.As(err, &se) || ctx.Err() != nil {
		return nil
	}
	return arrancarDaemonTry(ctx, endpoint)
}

// destinoEsElLocal dice si endpoint es el socket por defecto del daemon de
// esta máquina (el que arranca el agente de launchd con su raíz): ese y no otro
// puede arrancar try. Con -H o KLING_HOST a un socket de otra raíz, arrancar el
// agente real levantaría un daemon distinto del que se pidió.
func destinoEsElLocal(endpoint string) bool {
	p := strings.TrimPrefix(endpoint, "unix://")
	if p == "" {
		return true
	}
	return filepath.Clean(p) == filepath.Clean(transport.DefaultSocketPath())
}

// arrancarDaemonLocal arranca el daemon de esta máquina. En Mac, por el
// agente de launchd (instalándolo si quien está delante lo confirma). En
// Linux hace falta root: se dice la orden. Lo que dice va a stderr: stdout es
// del comando de try.
func arrancarDaemonLocal(ctx context.Context, endpoint string) error {
	if runtime.GOOS != "darwin" {
		return &errWithHint{
			err:  errors.New("the daemon is not running"),
			hint: "kling up -install, as root, installs and starts the systemd unit (the binary must be root-owned: " + dondeCopiar + ")",
		}
	}
	if !destinoEsElLocal(endpoint) {
		return &errWithHint{
			err:  fmt.Errorf("no daemon answers on %s, and it is not this machine's default socket", endpoint),
			hint: "start the daemon that listens there (kling daemon -root <its root>), or drop -H / KLING_HOST to use the local one",
		}
	}
	a, err := agenteDaemonMac()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "kling: the daemon is not running; starting it")
	return startMacDaemon(os.Stderr, a, endpoint, raizLocal())
}

// bajarImagenesLocal baja de la release de este kling el kernel y min.
func bajarImagenesLocal(ctx context.Context, endpoint string) error {
	if esRemoto(endpoint) {
		return errors.New("the daemon is remote: copy the image from a host that has it")
	}
	// La imagen va a la raíz con la que corre el daemon que contesta, no a la
	// por defecto: con otra raíz (KLING_ROOT del daemon) no la vería.
	root := raizLocal()
	ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if info, err := api.NewClient(endpoint).Info(ictx); err == nil && info.Root != "" {
		root = info.Root
	}
	return bajarImagenesMac(ctx, &upgrade.Fuente{VersionPropia: Version}, "v"+strings.TrimPrefix(Version, "v"), root, runtime.GOARCH, os.Stderr)
}

// elegirImagenTry devuelve la imagen con la que try crea el sandbox cuando no
// se da -image ni -from: la primera de imagenesTry que tenga el daemon. Si no
// tiene ninguna, baja min; si tampoco se puede, un error con una sola orden.
// Si no se puede ni listar (un daemon viejo, un fallo de red), se sigue con la
// de siempre y el daemon dirá lo suyo.
func elegirImagenTry(ctx context.Context, c *api.Client, endpoint string) (string, error) {
	elegir := func() (string, bool, error) {
		imgs, err := c.Images(ctx)
		if err != nil {
			return "", false, err
		}
		for _, want := range imagenesTry {
			for _, im := range imgs {
				if im.Name == want {
					return want, true, nil
				}
			}
		}
		return "", false, nil
	}
	n, ok, err := elegir()
	if err != nil {
		return tryDefaultImage, nil
	}
	if ok {
		return n, nil
	}
	fmt.Fprintln(os.Stderr, "kling: this daemon has no image yet; downloading min (about 10 MB)")
	if err := bajarImagenesTry(ctx, endpoint); err != nil {
		return "", sinImagenTry(err)
	}
	if n, ok, err = elegir(); err != nil || !ok {
		return "", sinImagenTry(errors.New("the daemon doesn't list the image that was just downloaded"))
	}
	return n, nil
}

func sinImagenTry(causa error) error {
	return &errWithHint{
		err:  fmt.Errorf("kling try needs an image and this daemon has none (%v)", causa),
		hint: "kling up   (downloads the kernel and min from this version's release; needs network. Offline: kling image copy min -from ssh://user@host)",
	}
}

// codigoTry es el código de salida de try cuando falla kling (no el comando):
// el del contrato (3 daemon viejo, 4 dorado obsoleto, 5 agente sin capacidad,
// 130 interrumpido, 2 uso) y, si el error no tiene uno, def.
func codigoTry(err error, def int) int {
	if c := codigoDeSalida(err); c != 0 && c != salidaError {
		return c
	}
	return def
}

// escribirPlantillaOverlay deja en root/images/overlay-template.ext4 la
// plantilla del disco escribible de las microVMs (mib MiB, sin journal,
// dispersa) escrita con internal/ext4, para no exigir mkfs.ext4 (e2fsprogs)
// en un Mac. El daemon la usa tal cual si existe. Si ya existe no se toca:
// puede ser la que formateó mkfs, y existir implica estar completa (se
// escribe en un temporal y se renombra).
func escribirPlantillaOverlay(root string, mib int) error {
	dir := filepath.Join(root, "images")
	dst := filepath.Join(dir, "overlay-template.ext4")
	if fileExists(dst) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Un temporal propio de cada llamada: con uno fijo, dos `kling up` a la vez
	// se pisaban (ENOENT al renombrar, o un fichero a medias publicado).
	f, err := os.CreateTemp(dir, ".overlay-template.ext4.tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = ext4.Write(f, ext4.NewDir(0o755, 0, 0, time.Now()), nil, ext4.Options{
		LostFound:  true,
		MinBlocks:  int64(mib) << 20 / ext4.BlockSize,
		InodeRatio: 16384, // un inodo por 16 KiB, como mke2fs
		ZeroHoles:  true,
	})
	if err == nil {
		err = f.Truncate(int64(mib) << 20)
	}
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// Link y no Rename: la primera que llega gana y la plantilla publicada no
	// cambia nunca; las demás dan por buena la que hay.
	if err := os.Link(tmp, dst); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// verificarSumas es `kling up -verify-sums`: lo que install.sh usa para
// comprobar la firma de SHA256SUMS con este kling (Go puro) cuando el usuario
// no tiene minisign. Las claves llegan de install.sh (separadas por espacios
// o comas); sin ellas, las de este binario. Sale con error si la firma no
// vale o no es de la etiqueta pedida.
func verificarSumas(sumas, firma, etiqueta, claves string) error {
	var ks []upgrade.ClavePublica
	for _, k := range strings.FieldsFunc(claves, func(r rune) bool { return r == ' ' || r == ',' || r == '\n' }) {
		c, err := upgrade.ParsearClavePublica(k)
		if err != nil {
			return fmt.Errorf("-key: %w", err)
		}
		ks = append(ks, c)
	}
	if len(ks) == 0 {
		ks = upgrade.ClavesRelease()
	}
	if len(ks) == 0 {
		return errors.New("this kling has no release keys built in and none were given with -key")
	}
	datos, err := os.ReadFile(sumas)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(firma)
	if err != nil {
		return err
	}
	com, err := upgrade.VerificarFirma(datos, sig, ks)
	if err != nil {
		return err
	}
	if t := upgrade.CampoComentario(com, "tag"); etiqueta != "" && t != etiqueta {
		return fmt.Errorf("the signature is for release %q, not %q: refusing it", t, etiqueta)
	}
	fmt.Printf("signature ok (%s)\n", etiqueta)
	return nil
}

// citarExec entrecomilla la ruta del binario para ExecStart si lleva algo que
// systemd interpretaría (espacios, comillas, %, $, \\).
func citarExec(exe string) string {
	if !strings.ContainsAny(exe, " \t\"'\\$%;") {
		return exe
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(exe) + `"`
}

// textoUnidad es la unidad systemd del daemon con kling en exe. Son las mismas
// líneas que packaging/kling.service (que make deploy instala), sin sus
// comentarios; un test las compara.
func textoUnidad(exe string) string {
	return `[Unit]
Description=kling - Firecracker microVM daemon
Documentation=https://github.com/juan52878911/kindling
After=network.target
ConditionPathExists=/dev/kvm

[Service]
Type=simple
EnvironmentFile=-/etc/default/kling
ExecStart=` + citarExec(exe) + ` daemon
Restart=on-failure
RestartSec=2
KillMode=process

[Install]
WantedBy=multi-user.target
`
}

const rutaUnidad = "/etc/systemd/system/kling.service"

// instalarUnidadSystemd escribe la unidad del daemon (kling up -install, como
// root) para quien instaló con install.sh y no tiene un checkout con
// `make deploy`. Una unidad que ya existe y es distinta no se pisa.
func instalarUnidadSystemd() error {
	if os.Geteuid() != 0 {
		return &errWithHint{
			err:  fmt.Errorf("writing %s needs root", rutaUnidad),
			hint: privilegedSelf("up", "-install"),
		}
	}
	exe, err := ejecutableReal()
	if err != nil {
		return err
	}
	if err := comprobarBinarioDeRoot(exe); err != nil {
		return err
	}
	body := textoUnidad(exe)
	if old, err := os.ReadFile(rutaUnidad); err == nil {
		if string(old) == body {
			fmt.Printf("unit:        %s already installed\n", rutaUnidad)
			return nil
		}
		fmt.Printf("unit:        %s already exists and differs: leaving it as it is\n", rutaUnidad)
		return nil
	}
	if err := escribirAtomico(rutaUnidad, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Printf("unit:        "+uiMarks().ok+" wrote %s\n", rutaUnidad)
	if out, err := lanzarctl("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dondeCopiar es la orden que deja el binario donde root puede fiarse de él.
const dondeCopiar = "sudo install -m 755 <path to kling> /usr/local/bin/kling && sudo /usr/local/bin/kling up -install"

// ordenUpInstall es la orden para escribir la unidad: la de copiar el binario
// a /usr/local/bin cuando el de ahora no es de root (la unidad lo rechazaría),
// y si no, este mismo binario con sudo.
func ordenUpInstall() string {
	exe, err := ejecutableReal()
	if err == nil {
		err = comprobarBinarioDeRoot(exe)
	}
	return ordenUpInstallSegun(err)
}

func ordenUpInstallSegun(errBinario error) string {
	if errBinario != nil {
		return dondeCopiar
	}
	return privilegedSelf("up", "-install")
}

// instalarEnLinux es `kling up -install` en Linux: firecracker, jailer y los
// usuarios si faltan, y la unidad. Con dryRun, solo dice qué haría. Se
// comprueba antes que se es root y que el binario es de root: así no se baja
// nada para acabar rechazando la unidad.
func instalarEnLinux(dryRun bool) error {
	if !dryRun {
		if os.Geteuid() != 0 {
			return &errWithHint{
				err:  fmt.Errorf("writing %s needs root", rutaUnidad),
				hint: ordenUpInstall(),
			}
		}
		exe, err := ejecutableReal()
		if err != nil {
			return err
		}
		if err := comprobarBinarioDeRoot(exe); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := prepararHostLinux(ctx, hostLinuxReal(dryRun)); err != nil {
		return err
	}
	if dryRun {
		fmt.Printf("would: write %s (this kling as ExecStart) and start it with systemctl enable --now kling\n", rutaUnidad)
		return nil
	}
	return instalarUnidadSystemd()
}

// comprobarBinarioDeRoot es lo que hay que cumplir para que una unidad de
// systemd ejecute exe como root: exe y cada directorio por encima son de root y
// no los escribe el grupo ni otros (salvo un directorio con bit pegajoso por
// encima, como /tmp, que no deja renombrar ni borrar lo ajeno). Con el kling de
// ~/.local/bin el usuario reescribiría el binario y sería root en el siguiente
// arranque. Como upgrade.bloquear con el directorio de estado.
func comprobarBinarioDeRoot(exe string) error {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	if real, err = filepath.Abs(real); err != nil {
		return err
	}
	rechazo := func(motivo string) error {
		return &errWithHint{
			err:  fmt.Errorf("refusing to run %s as root from the systemd unit: %s", real, motivo),
			hint: dondeCopiar,
		}
	}
	for d := real; ; d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return rechazo(d + ": cannot read its owner")
		}
		if st.Uid != 0 {
			return rechazo(fmt.Sprintf("%s is owned by uid %d, not root", d, st.Uid))
		}
		if fi.Mode().Perm()&0o022 != 0 && (d == real || fi.Mode()&os.ModeSticky == 0) {
			return rechazo(fmt.Sprintf("%s is writable by group or others (%v)", d, fi.Mode().Perm()))
		}
		if p := filepath.Dir(d); p == d {
			return nil
		}
	}
}

// escribirAtomico escribe datos en path vía un temporal del mismo directorio y
// un rename: quien lo lea ve el fichero entero o el que había.
func escribirAtomico(path string, datos []byte, modo os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(datos); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(modo); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
