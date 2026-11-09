//go:build !darwin

package machine

// Lo que hace el HOST alrededor de Firecracker en Linux. Cada función tiene su
// gemela en plataforma_vz.go (macOS, kling-vz); el resto del paquete llama a
// estas y no pregunta en qué sistema está. Aquí todo es lo de siempre: las
// funciones solo envuelven lo que antes estaba escrito en línea.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// putSinMontar: en Linux, intentarPut monta la imagen por loop.
const putSinMontar = false

// backendVMM es el VMM con el que arranca este binario.
const backendVMM = BackendFirecracker

// argsVMM son los argumentos de Firecracker que no dependen de la máquina,
// tras --api-sock (con jailer, tras "--"). El almacén MMDS y el cuerpo de
// una petición a su API van con el tope de api.MaxMMDSBytes: sus 50 KiB por
// defecto no daban para el entorno de la máquina (hasta 32 KiB, más su
// escape en JSON) junto a los secretos de sesión, y el PUT fallaba al
// arrancar o al inyectar.
func argsVMM() []string {
	tope := strconv.Itoa(api.MaxMMDSBytes)
	return []string{"--http-api-max-payload-size", tope, "--mmds-size-limit", tope}
}

// jailerPosible dice si esta plataforma tiene jailer. En Linux lo decide
// decidirJailer según haya binario y usuario sin privilegios (ver jailer.go).
const jailerPosible = true

// restaurarComparteMemoria: Firecracker restaura mapeando el mem.file del
// dorado MAP_PRIVATE, así que las copias comparten sus páginas limpias.
const restaurarComparteMemoria = true

// globoSinEstadisticas: Firecracker sí da las estadísticas del invitado.
const globoSinEstadisticas = false

// auditoriaEnElDaemon: en Linux el registro de auditoría del proxy lo escribe
// el daemon (root) y vive fuera del directorio de la máquina, que es del
// usuario sin privilegios del VMM (ver credaudit.go). Variable y no constante
// solo para que los tests de macOS puedan probar también este camino.
var auditoriaEnElDaemon = true

// dirsE2fsExtra son directorios donde buscar e2fsprogs además del PATH, /sbin
// y /usr/sbin. En Linux no hace falta ninguno.
var dirsE2fsExtra []string

// privilegiosPlataforma resuelve el usuario sin privilegios del VMM.
func privilegiosPlataforma(runAs string) (*Privileges, string) { return resolvePrivileges(runAs) }

// delegacionCgroups prepara el cgroup donde se limita la CPU de cada VMM.
func delegacionCgroups() (string, error) { return ensureDelegation() }

// puenteHostSoportado: en Linux no hay puentes al host (puente_host.go).
const puenteHostSoportado = false

// redAntesDeArrancar no hace nada en Linux: la red ya la montó el namespace
// antes de lanzar el VMM.
func (m *Manager) redAntesDeArrancar(ctx context.Context, c *fc.Client, id string) error { return nil }

// transporteCarpeta: en Linux no hay virtio-fs, y pedirlo es un error (sin
// pedirlo, FUSE como siempre).
func transporteCarpeta(pedido string) (string, error) {
	if pedido == share.TransportVirtioFS {
		return "", errors.New("virtio-fs shares need kling-hv (macOS); Firecracker serves live shares over FUSE")
	}
	return "", nil
}

// carpetasAntesDeArrancar no hace nada en Linux: Firecracker no tiene
// virtio-fs y las carpetas vivas van por FUSE (docs/compartir.md).
func (m *Manager) carpetasAntesDeArrancar(ctx context.Context, c *fc.Client, id string) {}

// attachVirtioFS: en Linux no hay virtio-fs.
func (m *Manager) attachVirtioFS(ctx context.Context, mc *api.Machine, tag int) (io.ReadWriteCloser, error) {
	return nil, fc.ErrNoVirtioFS
}

// fijarAuditoriaPlataforma: en Linux los proxies son del daemon (internal/net).
func fijarAuditoriaPlataforma(c credproxy.AuditConfig) { knet.SetCredAudit(c) }

// registrarCredencialesPlataforma: en Linux el proxy y el resolver son del
// daemon (internal/net); el VMM no interviene, así que c no se usa. El proxy
// escribe su registro de auditoría en auditPath.
func registrarCredencialesPlataforma(_ context.Context, _ *fc.Client, n *knet.Net, creds []credproxy.Credential, auditPath string, resolve credproxy.ResolveMachineFunc) error {
	return knet.SetCredentials(n, creds, auditPath, resolve)
}

// direccionCopiaLocked es por dónde llega el proxy del daemon al puerto port
// de la copia cp: la IP de su netns, cuyo DNAT lleva todos los puertos al
// invitado. Con m.mu tomado (cp es la entrada viva).
func direccionCopiaLocked(cp *api.Machine, port int) (string, error) {
	if cp.NetIndex <= 0 {
		return "", fmt.Errorf("machine %s has no network yet", cp.Name)
	}
	return net.JoinHostPort(knet.Plan(cp.NetIndex, cp.ID).NSIP, strconv.Itoa(port)), nil
}

// direccionListoLocked es a qué marca esperarPuertoPlataforma para saber si
// el puerto port de cp contesta (una arista depends con puerto): la misma
// dirección que usa el proxy. Con m.mu tomado.
func direccionListoLocked(cp *api.Machine, port int) (string, error) {
	return direccionCopiaLocked(cp, port)
}

// invalidarCopiaPlataforma corta las sesiones de todos los proxies hacia id.
func invalidarCopiaPlataforma(id string) int { return knet.InvalidarMaquina(id) }

// invalidarAgentePlataforma corta las sesiones hacia otras máquinas del
// proxy de la máquina de n.
func invalidarAgentePlataforma(n *knet.Net) int { return knet.InvalidarAgente(n) }

// invalidarEnlacesPlataforma corta las sesiones de enlace hacia ids en los
// proxies de enlace de todas las máquinas (internal/net).
func invalidarEnlacesPlataforma(ids ...string) int { return knet.InvalidarEnlaces(ids...) }

// maxVCPUsPlataforma: en Linux el núcleo no pone tope a vcpus; lo pone
// Firecracker (32) al configurar la máquina.
func maxVCPUsPlataforma() int { return 0 }

// invalidarOrigenPlataforma no hace nada en Linux: las sesiones que pidió una
// máquina las corta invalidarAgente (su proxy es del daemon).
func invalidarOrigenPlataforma(string) int { return 0 }

// iniciarBroker no hace nada en Linux: las aristas las sirve el daemon en el
// netns de cada nodo (internal/net), sin broker (broker.go).
func (m *Manager) iniciarBroker() {}

// cerrarBroker: ver iniciarBroker.
func (m *Manager) cerrarBroker() {}

// enviarGrafoPlataforma no hace nada en Linux: SetGraph ya lo montó todo en
// el netns del nodo.
func enviarGrafoPlataforma(context.Context, *Manager, string) error { return nil }

// esperarPuertoPlataforma espera a que addr (la IP del netns del destino)
// acepte conexiones: un nodo recién despertado tarda un poco en volver a
// escuchar.
func esperarPuertoPlataforma(ctx context.Context, _ *Manager, _, addr string, _ int) error {
	var d net.Dialer
	for {
		intento, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		c, err := d.DialContext(intento, "tcp", addr)
		cancel()
		if err == nil {
			_ = c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("port %s didn't answer: %w", addr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// abrirReenvios no hace nada en Linux: el host alcanza al invitado por la IP
// del veth, sin reenvíos.
func (m *Manager) abrirReenvios(ctx context.Context, c *fc.Client, id string) error { return nil }

// copiarDisco copia un disco conservando su dispersión. --sparse=always: el
// overlay es disperso y copiarlo denso destruiría lo que hace que una máquina
// cueste ~8 MB en vez de 512.
func copiarDisco(ctx context.Context, src, dst string) ([]byte, error) {
	return exec.CommandContext(ctx, "cp", "--sparse=always", src, dst).CombinedOutput()
}

// clonarDisco copia un volumen para un snapshot o un restore y dice cómo:
// "reflink" si el sistema de ficheros comparte bloques (XFS con reflink, Btrfs),
// que es instantáneo y no ocupa nada hasta que las copias divergen; "copy" si
// hubo que copiar entero, disperso.
//
// --reflink=always SIN --sparse: GNU cp rechaza --reflink con cualquier
// --sparse que no sea el de por defecto, y con --sparse=always el primer
// intento fallaría siempre y todo acabaría en copia completa sin decirlo. Un
// reflink conserva los huecos igualmente: comparte las extensiones, no las
// rellena. antesDeCopiar decide si una copia completa cabe; su error es el que
// se devuelve.
func clonarDisco(ctx context.Context, src, dst string, antesDeCopiar func() error) (string, error) {
	if _, err := exec.CommandContext(ctx, "cp", "--reflink=always", src, dst).CombinedOutput(); err == nil {
		return "reflink", nil
	}
	_ = os.Remove(dst)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := antesDeCopiar(); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, "cp", "--sparse=always", src, dst).CombinedOutput(); err != nil {
		return "", fmt.Errorf("copying %s: %s", filepath.Base(src), conSalida(err, out))
	}
	return "copy", nil
}

// perforarHuecos devuelve al disco las páginas a cero de un volcado de
// memoria (ver Freeze).
func perforarHuecos(ctx context.Context, path string) ([]byte, error) {
	return exec.CommandContext(ctx, "fallocate", "--dig-holes", path).CombinedOutput()
}

// e2fsCmd prepara una orden de e2fsprogs. En Linux van por el PATH, como
// siempre.
func e2fsCmd(ctx context.Context, nombre string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, nombre, args...)
}

// uidNadieE2fsck es el usuario con el que corre e2fsck sobre un disco que es
// de root: nobody/nogroup en casi todo Linux. Variable para los tests.
var uidNadieE2fsck = 65534

// memE2fsck es el tope de espacio de direcciones de e2fsck (RLIMIT_AS). Un
// superbloque que dice tener millones de grupos le haría reservar lo que
// pidiera; las tablas de un volumen sano de 1 TiB son unos cientos de MiB.
const memE2fsck = 2 << 30

// confinarE2fs quita a una orden de e2fsprogs que va a leer un disco escrito
// por el invitado (f, ya abierto, que la orden recibe como /dev/fd/3) todo lo
// que no necesita para eso:
//
//   - el uid 0: corre con el dueño del disco si no es root (el usuario del
//     VMM, que ya puede escribirlo: con -run-as el overlay y los volúmenes
//     son suyos), o con nobody. A un disco de root se le cede el dueño (por
//     descriptor) mientras dura y se le devuelve después: abrir /dev/fd/3
//     vuelve a comprobar permisos sobre el inodo, así que el descriptor solo
//     no basta. Sin grupos suplementarios, y al dejar de ser root pierde
//     todas las capacidades.
//   - la red, y la vista de los procesos e IPC del host (namespaces nuevos de
//     red, PID e IPC).
//   - memoria sin tope: prlimit (util-linux, como setpriv) le pone memE2fsck.
//
// El plazo lo pone quien la lanza (plazoE2fsck). Con el daemon sin root no se
// cambia de usuario (ya no es root) ni de namespaces (harían falta los de
// usuario, que hay distribuciones que bloquean): solo el tope de memoria.
// Devuelve con qué deshacer lo que tocó del disco.
func confinarE2fs(c *exec.Cmd, f *os.File) (func(), error) {
	nada := func() {}
	if c.Err != nil {
		return nada, nil
	}
	if bin, err := exec.LookPath("prlimit"); err == nil {
		c.Args = append([]string{bin, "--as=" + strconv.Itoa(memE2fsck), "--", c.Path}, c.Args[1:]...)
		c.Path = bin
	}
	if os.Geteuid() != 0 {
		return nada, nil
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat %s: no owner", f.Name())
	}
	uid, gid := int(st.Uid), int(st.Gid)
	soloLectura := descriptorSoloLectura(f)
	modo := fi.Mode().Perm()
	var deshacer []func()
	// Quien solo lee (debugfs sin -w) no necesita que se le ceda el disco si
	// ya lo puede leer nobody, por los demás o por un grupo que no sea el de
	// root: así no se toca el dueño de una imagen base que otros VMM abren
	// por grupo mientras tanto (darLecturaVMM).
	switch {
	case uid != 0:
		// El dueño (el usuario del VMM): ya puede con él.
	case soloLectura && modo&0o004 != 0:
		uid, gid = uidNadieE2fsck, uidNadieE2fsck
	case soloLectura && modo&0o040 != 0 && gid != 0:
		uid = uidNadieE2fsck
	default:
		uid, gid = uidNadieE2fsck, uidNadieE2fsck
		if err := f.Chown(uid, gid); err != nil {
			return nil, fmt.Errorf("handing %s to e2fsck: %w", filepath.Base(f.Name()), err)
		}
		deshacer = append(deshacer, func() { _ = f.Chown(int(st.Uid), int(st.Gid)) })
	}
	// e2fsck -p escribe: el dueño necesita rw aunque el modo no lo diera.
	if !soloLectura && modo&0o600 != 0o600 {
		if err := f.Chmod(modo | 0o600); err == nil {
			deshacer = append(deshacer, func() { _ = f.Chmod(modo) })
		}
	}
	c.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}},
		Cloneflags: syscall.CLONE_NEWNET | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC,
	}
	return func() {
		for i := len(deshacer) - 1; i >= 0; i-- {
			deshacer[i]()
		}
	}, nil
}

// descriptorSoloLectura dice si f se abrió O_RDONLY. Ante la duda, no: se
// trata como si fuera a escribir.
func descriptorSoloLectura(f *os.File) bool {
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	flags := -1
	_ = rc.Control(func(fd uintptr) {
		r, _, e := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		if e == 0 {
			flags = int(r)
		}
	})
	return flags >= 0 && flags&syscall.O_ACCMODE == syscall.O_RDONLY
}

// checkPresionPlataforma no añade nada en Linux: allí la presión la mide PSI
// en checkPressure.
func (m *Manager) checkPresionPlataforma() error { return nil }

// minDiscoLibrePlataforma: 2 GiB en Linux, donde el swap (si lo hay) no
// crece a costa del disco de datos.
const minDiscoLibrePlataforma = 2048

// sondearPuertoVMM: en Linux no hay /kling/probe al que preguntar
// (ProbeGuestPort no llega aquí sin reenvíos); se deja la pregunta tal cual.
func sondearPuertoVMM(ctx context.Context, sock string, port int, _ bool) (bool, error) {
	return fc.New(sock).KlingProbe(ctx, port)
}

// enterRestore y enterBoot: en Linux restaurar y arrancar en frío comparten
// la puerta de arranque (launch.go); la tormenta que cuelga un host anidado es
// la misma.
func (m *Manager) enterRestore(ctx context.Context) (func(), error) { return m.enterLaunch(ctx) }

func (m *Manager) enterBoot(ctx context.Context) (func(), error) { return m.enterLaunch(ctx) }

// apretarAntesDePausar: en Linux no. Una pausada retiene lo que el invitado
// tocó, casi todo caché del mem.file COMPARTIDA con las demás copias, y el
// globo la convertiría en memoria privada al volver (ver squeezeLocked).
func (m *Manager) apretarAntesDePausar(context.Context, *api.Machine) int { return 0 }

// lanzamientoPlataforma es el tope de encendidos simultáneos propio de la
// plataforma; 0 = el cálculo general de maxParallelLaunch.
func lanzamientoPlataforma() int { return 0 }

// memoriaVMM es la memoria de host que ocupa el VMM de una máquina (PSS).
func memoriaVMM(pid int, sock string) int64 { return pssMiB(pid) }

// rssVMM es el RSS del VMM, la cifra que cae cuando el globo devuelve páginas.
func rssVMM(pid int, sock string) int { return procRSSMiB(pid) }

// memoriaHost es la memoria disponible y libre del anfitrión, en MiB.
func memoriaHost() (available, free int64) { return hostMemMiB() }

// memoriaTotalPlataforma: 0 en Linux, donde nadie la pide (el nivel pausada
// automático es solo del Mac).
func memoriaTotalPlataforma() int64 { return 0 }

// entornoVMM: Firecracker no necesita nada más que el entorno del daemon (su
// confinamiento es el jailer).
func (m *Manager) entornoVMM() []string { return nil }

// entornoProceso: en Linux el VMM hereda el entorno del daemon (nil = el de
// este proceso), más extra si lo hay.
func entornoProceso(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	return append(os.Environ(), extra...)
}

// techoDelDaemon es el cpu_pct de una máquina que no lo pidió (ni por
// flag, ni su dorado, ni su receta, ni la configuración). En Linux, media vCPU
// (defaultCPUPct): el cgroup no cuesta nada al invitado.
func techoDelDaemon(vcpus int) int { return defaultCPUPct }

// versionMacOS es "" fuera de macOS: el dorado no depende del sistema.
func versionMacOS() string { return "" }
