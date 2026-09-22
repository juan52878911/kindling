// vzproto: prototipo de backend nativo de kindling sobre Virtualization.framework.
//
// Mide, en un Mac Apple Silicon y SIN VM intermedia ni virtualización anidada,
// lo que kindling mide en Linux con Firecracker: arranque en frío, congelar
// (guardar estado), descongelar (restaurar), latencia hasta que el invitado
// responde, y huella física de memoria del proceso anfitrión en cada fase.
//
// Usa EXACTAMENTE el mismo kernel (vmlinux 6.1 de Firecracker, aarch64), la
// misma imagen mínima (min.ext4 con /sbin/overlay-init) y el mismo esquema de
// discos (raíz de solo lectura en vda, overlay en vdb) que el daemon real. Lo
// único que cambia es la consola: Firecracker expone un 8250 (ttyS0) y
// Virtualization.framework un virtio-console (hvc0).
//
//	vzproto boot    -save estado.vzs          arranca, espera al shell, guarda
//	vzproto restore -state estado.vzs -n 5    restaura N copias y mide cada una
//
// Requiere firmar el binario con el entitlement com.apple.security.virtualization
// (ver build.sh).
package main

/*
#include <libproc.h>
#include <sys/resource.h>
#include <unistd.h>

#include <string.h>
#include <stdlib.h>

// footprint devuelve phys_footprint (lo que Activity Monitor llama "Memory")
// y resident_size del proceso pid, en bytes. -1 si falla.
static int footprint(int pid, long long *phys, long long *rss) {
	struct rusage_info_v4 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)&ri) != 0) return -1;
	*phys = (long long)ri.ri_phys_footprint;
	*rss = (long long)ri.ri_resident_size;
	return 0;
}

// vz_helpers rellena out con los pids de los procesos auxiliares de
// Virtualization.framework (com.apple.Virtualization.VirtualMachine). La memoria
// del invitado vive AHÍ, no en el proceso que crea la VM: medir solo el propio
// proceso da 18 MiB para una máquina que en realidad ocupa 60.
static int vz_helpers(int *out, int max) {
	int n = proc_listallpids(NULL, 0);
	if (n <= 0) return 0;
	pid_t *pids = calloc(n + 64, sizeof(pid_t));
	n = proc_listallpids(pids, (n + 64) * sizeof(pid_t));
	int k = 0;
	char path[PROC_PIDPATHINFO_MAXSIZE];
	for (int i = 0; i < n && k < max; i++) {
		if (pids[i] <= 0) continue;
		if (proc_pidpath(pids[i], path, sizeof path) <= 0) continue;
		if (strstr(path, "com.apple.Virtualization.VirtualMachine")) out[k++] = pids[i];
	}
	free(pids);
	return k;
}
*/
import "C"

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// readyMarker es lo que imprime minimal-init.sh cuando la imagen no tiene
// /entrypoint: es el instante en que el invitado está montado, pivotado y con
// un shell escuchando en la consola. Equivale al "waitReady" de kindling.
const readyMarker = "cayendo a shell"

// cmdline replica bootArgsBase de internal/machine/manager.go cambiando solo
// la consola (hvc0 en vez de ttyS0) y quitando pci=off: en Virtualization.
// framework los virtio van por PCI.
const cmdline = "console=hvc0 reboot=k panic=1 root=/dev/vda ro init=/sbin/overlay-init"

type opts struct {
	kernel, rootfs, overlay string
	memMiB                  int
	cpus                    int
	verbose                 bool
	cmdline                 string
	ready                   string
	workdir                 string
	noclone                 bool // reutilizar workdir/overlay-N.ext4 tal cual (mismo inodo)
	noBalloon               bool
	idPath                  string // identificador de máquina; el estado guardado exige el mismo
	layer                   string // capa de servicio (kling.layer=/dev/vdc), opcional
	ip, gw                  string // red NAT con IP estática por cmdline, como hace kindling
	sameNet                 bool   // réplicas con la MISMA IP/MAC que el estado (config idéntica)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "uso: vzproto boot|restore|fleet [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "boot":
		err = cmdBoot(os.Args[2:])
	case "restore":
		err = cmdRestore(os.Args[2:])
	case "fleet":
		err = cmdFleet(os.Args[2:])
	case "maxvms":
		err = cmdMaxVMs(os.Args[2:])
	default:
		err = fmt.Errorf("comando desconocido: %s", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func commonFlags(fs *flag.FlagSet) *opts {
	o := &opts{}
	fs.StringVar(&o.kernel, "kernel", "guest/vmlinux", "kernel arm64: el vmlinux aarch64 de Firecracker ya es un Image (cabecera ARMd)")
	fs.StringVar(&o.rootfs, "rootfs", "guest/min.ext4", "imagen raíz (solo lectura)")
	fs.StringVar(&o.overlay, "overlay", "guest/overlay-64.ext4", "plantilla del overlay; se clona por máquina")
	fs.IntVar(&o.memMiB, "mem", 256, "MiB de RAM (kindling: 256)")
	fs.IntVar(&o.cpus, "cpus", 1, "vCPUs (kindling: 1)")
	fs.BoolVar(&o.verbose, "v", false, "volcar la consola del invitado")
	fs.StringVar(&o.cmdline, "cmdline", cmdline, "línea de comandos del kernel")
	fs.StringVar(&o.ready, "ready", readyMarker, "línea de consola que marca 'invitado listo'")
	fs.StringVar(&o.workdir, "workdir", "", "directorio de trabajo (por defecto uno temporal)")
	fs.BoolVar(&o.noclone, "noclone", false, "no clonar el overlay: usar workdir/overlay-N.ext4 si ya existe")
	fs.BoolVar(&o.noBalloon, "no-balloon", false, "sin dispositivo de globo")
	fs.StringVar(&o.layer, "layer", "", "capa de servicio *.layer.ext4 (se monta como /dev/vdc, kling.layer=)")
	fs.StringVar(&o.ip, "ip", "", "IP estática del invitado en la NAT de vz (p.ej. 192.168.64.222); activa la red")
	fs.StringVar(&o.gw, "gw", "192.168.64.1", "puerta de enlace de la NAT de vz")
	fs.BoolVar(&o.sameNet, "same-net", false, "todas las réplicas con la IP y MAC del estado guardado (el restore exige config idéntica)")
	return o
}

// ipFor da a la máquina idx su IP: la base con el último octeto sumado.
func (o *opts) ipFor(idx int) string {
	if o.ip == "" {
		return ""
	}
	if o.sameNet {
		return o.ip
	}
	a := net.ParseIP(o.ip).To4()
	if a == nil {
		return o.ip
	}
	b := make(net.IP, 4)
	copy(b, a)
	b[3] += byte(idx)
	return b.String()
}

// machine es una microVM con su consola pinchada.
type machine struct {
	ip      string // IP de ESTA máquina (base + índice)
	vm      *vz.VirtualMachine
	cfg     *vz.VirtualMachineConfiguration
	toGuest *os.File // escribimos aquí -> el invitado lo lee
	lines   chan string
	overlay string
	verbose bool
}

func (o *opts) newMachine(idx int) (*machine, error) {
	if o.workdir == "" {
		d, err := os.MkdirTemp("", "vzproto-")
		if err != nil {
			return nil, err
		}
		o.workdir = d
	}
	// El overlay se clona por máquina como hace kindling con su plantilla
	// (reflink en APFS: cp -c es instantáneo y no duplica bloques).
	overlay := filepath.Join(o.workdir, fmt.Sprintf("overlay-%d.ext4", idx))
	if _, err := os.Stat(overlay); err != nil || !o.noclone {
		if out, err := exec.Command("cp", "-c", o.overlay, overlay).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("clonar overlay: %v: %s", err, out)
		}
	}

	cl := o.cmdline
	if o.layer != "" {
		cl += " kling.layer=/dev/vdc"
	}
	ip := o.ipFor(idx)
	if ip != "" {
		cl += fmt.Sprintf(" ip=%s::%s:255.255.255.0::eth0:off", ip, o.gw)
	}
	boot, err := vz.NewLinuxBootLoader(o.kernel, vz.WithCommandLine(cl))
	if err != nil {
		return nil, fmt.Errorf("bootloader: %w", err)
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, uint(o.cpus), uint64(o.memMiB)<<20)
	if err != nil {
		return nil, err
	}

	// Identidad de la máquina. Virtualization.framework genera una aleatoria
	// por configuración y se niega a restaurar un estado con otra: es el
	// "invalid argument" que sale si no se persiste. Va junto al estado, como
	// el snap.file de Firecracker lleva la config de la máquina.
	var mid *vz.GenericMachineIdentifier
	if b, err := os.ReadFile(o.idPath); err == nil {
		if mid, err = vz.NewGenericMachineIdentifierWithData(b); err != nil {
			return nil, err
		}
	} else {
		if mid, err = vz.NewGenericMachineIdentifier(); err != nil {
			return nil, err
		}
		if o.idPath != "" {
			if err := os.WriteFile(o.idPath, mid.DataRepresentation(), 0o644); err != nil {
				return nil, err
			}
		}
	}
	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(mid))
	if err != nil {
		return nil, err
	}
	cfg.SetPlatformVirtualMachineConfiguration(platform)

	// Consola: dos tuberías. guestIn la lee el invitado; guestOut la escribe.
	guestInR, guestInW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	guestOutR, guestOutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	serial, err := vz.NewFileHandleSerialPortAttachment(guestInR, guestOutW)
	if err != nil {
		return nil, err
	}
	console, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serial)
	if err != nil {
		return nil, err
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})

	// Discos: vda raíz ro, vdb overlay rw. Mismo orden que el daemon.
	rootAtt, err := vz.NewDiskImageStorageDeviceAttachment(o.rootfs, true)
	if err != nil {
		return nil, fmt.Errorf("rootfs: %w", err)
	}
	rootDev, err := vz.NewVirtioBlockDeviceConfiguration(rootAtt)
	if err != nil {
		return nil, err
	}
	ovAtt, err := vz.NewDiskImageStorageDeviceAttachment(overlay, false)
	if err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
	}
	ovDev, err := vz.NewVirtioBlockDeviceConfiguration(ovAtt)
	if err != nil {
		return nil, err
	}
	disks := []vz.StorageDeviceConfiguration{rootDev, ovDev}
	if o.layer != "" {
		layAtt, err := vz.NewDiskImageStorageDeviceAttachment(o.layer, true)
		if err != nil {
			return nil, fmt.Errorf("layer: %w", err)
		}
		layDev, err := vz.NewVirtioBlockDeviceConfiguration(layAtt)
		if err != nil {
			return nil, err
		}
		disks = append(disks, layDev)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(disks)

	// Red: NAT de Virtualization.framework. La MAC se persiste junto al
	// identificador porque el estado guardado exige la misma configuración.
	if o.ip != "" {
		nat, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return nil, err
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
		if err != nil {
			return nil, err
		}
		macPath := o.idPath + ".mac"
		if idx > 0 && !o.sameNet {
			macPath = fmt.Sprintf("%s.mac%d", o.idPath, idx)
		}
		var mac *vz.MACAddress
		if b, err := os.ReadFile(macPath); o.idPath != "" && err == nil {
			hw, err := net.ParseMAC(strings.TrimSpace(string(b)))
			if err != nil {
				return nil, err
			}
			if mac, err = vz.NewMACAddress(hw); err != nil {
				return nil, err
			}
		} else {
			if mac, err = vz.NewRandomLocallyAdministeredMACAddress(); err != nil {
				return nil, err
			}
			if o.idPath != "" {
				os.WriteFile(macPath, []byte(mac.String()), 0o644)
			}
		}
		nic.SetMACAddress(mac)
		cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})
	}

	// Globo de memoria: es lo que `kling squeeze` usa para devolver RAM.
	if !o.noBalloon {
		balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
		if err != nil {
			return nil, err
		}
		cfg.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})
	}

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("configuración inválida: %v", err)
	}
	if ok, err := cfg.ValidateSaveRestoreSupport(); !ok || err != nil {
		return nil, fmt.Errorf("esta configuración no admite guardar/restaurar: %v", err)
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, err
	}

	m := &machine{ip: ip, vm: vm, cfg: cfg, toGuest: guestInW, lines: make(chan string, 4096), overlay: overlay, verbose: o.verbose}
	go func() {
		sc := bufio.NewScanner(guestOutR)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if m.verbose {
				fmt.Fprintf(os.Stderr, "  [guest %d] %s\n", idx, l)
			}
			select {
			case m.lines <- l:
			default: // no bloquear al invitado si nadie lee
			}
		}
	}()
	return m, nil
}

// waitLine espera a que la consola emita una línea que contenga needle.
func (m *machine) waitLine(needle string, timeout time.Duration) (time.Duration, error) {
	t0 := time.Now()
	deadline := time.After(timeout)
	for {
		select {
		case l := <-m.lines:
			if strings.Contains(l, needle) {
				return time.Since(t0), nil
			}
		case <-deadline:
			return 0, fmt.Errorf("timeout %s esperando %q en la consola", timeout, needle)
		}
	}
}

// ping manda un comando al shell del invitado y mide hasta ver su respuesta.
// Es el análogo de "primera petición HTTP tras despertar" en kindling.
func (m *machine) ping(tag string, timeout time.Duration) (time.Duration, error) {
	// drenar lo que haya pendiente para no casar con ecos viejos
	for len(m.lines) > 0 {
		<-m.lines
	}
	t0 := time.Now()
	if _, err := fmt.Fprintf(m.toGuest, "echo PONG-%s\n", tag); err != nil {
		return 0, err
	}
	// el eco del terminal repite el comando; queremos la SALIDA, que va sola en su línea
	deadline := time.After(timeout)
	for {
		select {
		case l := <-m.lines:
			if strings.TrimSpace(l) == "PONG-"+tag {
				return time.Since(t0), nil
			}
		case <-deadline:
			return 0, fmt.Errorf("timeout esperando PONG-%s", tag)
		}
	}
}

func (m *machine) waitState(want vz.VirtualMachineState, timeout time.Duration) error {
	if m.vm.State() == want {
		return nil
	}
	deadline := time.After(timeout)
	ch := m.vm.StateChangedNotify()
	for {
		select {
		case s := <-ch:
			if s == want {
				return nil
			}
			if s == vz.VirtualMachineStateError {
				return fmt.Errorf("la VM entró en estado de error")
			}
		case <-deadline:
			return fmt.Errorf("timeout esperando estado %v (actual %v)", want, m.vm.State())
		}
	}
}

// helpersBaseline son los procesos auxiliares de Virtualization que ya existían
// al arrancar (p. ej. la VM de Lima): no son nuestros y se excluyen.
var helpersBaseline = map[int]bool{}

func listHelpers() []int {
	buf := make([]C.int, 256)
	n := int(C.vz_helpers(&buf[0], C.int(len(buf))))
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, int(buf[i]))
	}
	return out
}

func init() {
	for _, p := range listHelpers() {
		helpersBaseline[p] = true
	}
}

// helperCount dice cuántos procesos auxiliares nuestros hay vivos ahora.
func helperCount() int {
	n := 0
	for _, p := range listHelpers() {
		if !helpersBaseline[p] {
			n++
		}
	}
	return n
}

// mem devuelve (phys_footprint, resident) en MiB sumando ESTE proceso y los
// auxiliares de Virtualization que hemos creado nosotros. phys_footprint es lo
// que macOS de verdad ha comprometido por la VM: es el equivalente al RSS del
// proceso firecracker que mide kindling.
func mem() (float64, float64) {
	var tp, tr float64
	add := func(pid int) {
		var phys, rss C.longlong
		if C.footprint(C.int(pid), &phys, &rss) == 0 {
			tp += float64(phys)
			tr += float64(rss)
		}
	}
	add(os.Getpid())
	for _, p := range listHelpers() {
		if !helpersBaseline[p] {
			add(p)
		}
	}
	return tp / (1 << 20), tr / (1 << 20)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"vzproto","version":"0"}}}`

// waitHealthz sondea /healthz del puente hasta que responde: es el instante en
// que el gateway podría empezar a hablarle.
func waitHealthz(ip string, timeout time.Duration) (time.Duration, error) {
	t0 := time.Now()
	// Timeout corto: tras un restore el primer SYN puede perderse mientras la
	// NAT vuelve a aprender la MAC; con 500 ms cada intento fallido inflaba la
	// medida medio segundo. Se cuenta cuántos fallan para verlo.
	c := &http.Client{Timeout: 100 * time.Millisecond}
	fails := 0
	for time.Since(t0) < timeout {
		resp, err := c.Get("http://" + ip + ":8080/healthz")
		if err == nil {
			resp.Body.Close()
			if fails > 0 {
				fmt.Printf("    (healthz: %d intentos fallidos antes de responder)\n", fails)
			}
			return time.Since(t0), nil
		}
		fails++
		time.Sleep(10 * time.Millisecond)
	}
	return 0, fmt.Errorf("el puente no responde en %s tras %s", ip, timeout)
}

// initialize manda un initialize MCP al puente (sesión nueva -> proceso node
// nuevo) y mide la respuesta completa.
func initialize(ip string) (time.Duration, string, error) {
	d, note, sid, err := mcpPost(ip, initializeBody, "")
	lastSession = sid
	return d, note, err
}

// lastSession es el Mcp-Session-Id que devolvió el último initialize. Se
// guarda junto al estado: tras descongelar, el gateway sigue hablando con la
// sesión que ya existía dentro del snapshot, no abre otra.
var lastSession string

const toolsListBody = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`

// httpClient reutiliza conexiones: el Transport por defecto solo guarda 2 por
// host y con 64 clientes abre y cierra sin parar hasta agotar los puertos
// efímeros del anfitrión ("can't assign requested address"). Eso mide al
// generador de carga, no al puente.
var httpClient = &http.Client{
	Timeout: 3 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	},
}

func mcpPost(ip, body, session string) (time.Duration, string, string, error) {
	return mcpPostRaw(ip, body, session)
}

// mcpPostRaw hace la petición y devuelve (latencia, nota, Mcp-Session-Id). La
// nota empieza por "http <código>" y lleva "rpc-error" si el JSON-RPC trae error:
// un 200 con error dentro no es una petición servida.
func mcpPostRaw(ip, body, session string) (time.Duration, string, string, error) {
	d, note, sid, _, err := mcpPostFull(ip, body, session)
	return d, note, sid, err
}

// mcpPostRawSample devuelve (latencia, nota, recorte del cuerpo).
func mcpPostRawSample(ip, body, session string) (time.Duration, string, string, error) {
	d, note, _, sample, err := mcpPostFull(ip, body, session)
	return d, note, sample, err
}

func mcpPostFull(ip, body, session string) (time.Duration, string, string, string, error) {
	t0 := time.Now()
	req, _ := http.NewRequest("POST", "http://"+ip+":8080/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	sample := strings.TrimSpace(string(b))
	if len(sample) > 200 {
		sample = sample[:200] + "…"
	}
	note := fmt.Sprintf("http %d, %d bytes", resp.StatusCode, len(b))
	if resp.StatusCode != 200 {
		snip := strings.TrimSpace(string(b))
		if len(snip) > 120 {
			snip = snip[:120]
		}
		note += " «" + snip + "»"
	}
	var rpc struct {
		Error *json.RawMessage `json:"error"`
	}
	// Puede venir como SSE ("data: {...}"); se busca la primera línea JSON.
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "data:")
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			if json.Unmarshal([]byte(line), &rpc) == nil && rpc.Error != nil {
				note += " rpc-error " + string(*rpc.Error)
			}
			break
		}
	}
	return time.Since(t0), note, resp.Header.Get("Mcp-Session-Id"), sample, nil
}

type result struct {
	Phase     string  `json:"phase"`
	Ms        float64 `json:"ms"`
	PhysMiB   float64 `json:"phys_mib"`
	RssMiB    float64 `json:"rss_mib"`
	FileBytes int64   `json:"file_bytes,omitempty"`
	Note      string  `json:"note,omitempty"`
}

var (
	resultsMu sync.Mutex
	results   []result
)

func record(phase string, d time.Duration, note string, fileBytes int64) {
	phys, rss := mem()
	r := result{Phase: phase, Ms: ms(d), PhysMiB: phys, RssMiB: rss, FileBytes: fileBytes, Note: note}
	resultsMu.Lock()
	results = append(results, r)
	resultsMu.Unlock()
	line := fmt.Sprintf("%-32s %9.1f ms   phys %7.1f MiB   rss %7.1f MiB  [%d aux]", phase, r.Ms, phys, rss, helperCount())
	if fileBytes > 0 {
		line += fmt.Sprintf("   fichero %.1f MiB", float64(fileBytes)/(1<<20))
	}
	if note != "" {
		line += "   " + note
	}
	fmt.Println(line)
}

func dumpJSON(path string) error {
	if path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(results, "", "  ")
	return os.WriteFile(path, b, 0o644)
}

// ---------------------------------------------------------------- boot

func cmdBoot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	o := commonFlags(fs)
	save := fs.String("save", "", "tras arrancar, pausa y guarda el estado aquí")
	keep := fs.Duration("keep", 0, "mantener la VM viva este tiempo antes de parar (para medir desde fuera)")
	idle := fs.Duration("idle", 2*time.Second, "reposo antes de medir memoria estable")
	jsonOut := fs.String("json", "", "escribir resultados en este fichero")
	execCmd := fs.String("exec", "", "comando de shell a cronometrar dentro del invitado una vez listo")
	balloonB := fs.Int("balloon", 0, "antes de guardar, inflar el globo hasta dejar al invitado en estos MiB (squeeze antes de congelar)")
	pauseFor := fs.Duration("pause", 0, "pausar la VM este tiempo (nivel 'pausada en RAM'), medir su huella y reanudar")
	fs.Parse(args)

	if *save != "" {
		o.idPath = *save + ".id"
		os.Remove(o.idPath)
		os.Remove(o.idPath + ".mac")
	}
	record("baseline (proceso vacío)", 0, "", 0)

	t0 := time.Now()
	m, err := o.newMachine(0)
	if err != nil {
		return err
	}
	record("configurar VM", time.Since(t0), "", 0)

	t0 = time.Now()
	if err := m.vm.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := m.waitState(vz.VirtualMachineStateRunning, 10*time.Second); err != nil {
		return err
	}
	record("Start() -> running", time.Since(t0), "", 0)

	if o.ip != "" {
		d, err := waitHealthz(o.ip, 120*time.Second)
		if err != nil {
			return err
		}
		record("arranque en frío -> puente escuchando", time.Since(t0), "kernel + init + kling-bridge", 0)
		_ = d
		d, note, err := initialize(o.ip)
		if err != nil {
			return err
		}
		record("1er initialize (node en frío)", d, note, 0)
		d, note, _, err = mcpPost(o.ip, toolsListBody, lastSession)
		if err != nil {
			return err
		}
		record("tools/list en la misma sesión (caliente)", d, note, 0)
		if *save != "" {
			os.WriteFile(*save+".session", []byte(lastSession), 0o644)
		}
	} else {
		if _, err := m.waitLine(o.ready, 60*time.Second); err != nil {
			return err
		}
		record("arranque en frío -> shell listo", time.Since(t0), "kernel + init", 0)

		d, err := m.ping("boot", 10*time.Second)
		if err != nil {
			return err
		}
		record("primer comando respondido", d, "", 0)
	}

	if *execCmd != "" {
		for len(m.lines) > 0 {
			<-m.lines
		}
		t0 = time.Now()
		fmt.Fprintf(m.toGuest, "%s; echo EXEC-DONE\n", *execCmd)
		deadline := time.After(5 * time.Minute)
	waitExec:
		for {
			select {
			case l := <-m.lines:
				if strings.TrimSpace(l) == "EXEC-DONE" {
					break waitExec
				}
			case <-deadline:
				return fmt.Errorf("timeout esperando a -exec")
			}
		}
		record("-exec en el invitado", time.Since(t0), *execCmd, 0)
	}

	time.Sleep(*idle)
	record(fmt.Sprintf("en marcha, %s de reposo", *idle), 0, "", 0)

	if *keep > 0 {
		fmt.Printf("pid %d — VM viva durante %s\n", os.Getpid(), *keep)
		time.Sleep(*keep)
	}

	if *pauseFor > 0 {
		t0 = time.Now()
		if err := m.vm.Pause(); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		if err := m.waitState(vz.VirtualMachineStatePaused, 10*time.Second); err != nil {
			return err
		}
		record("Pause() (nivel pausada)", time.Since(t0), "", 0)
		step := *pauseFor / 3
		for i := 1; i <= 3; i++ {
			time.Sleep(step)
			record(fmt.Sprintf("pausada %s", step*time.Duration(i)), 0, "", 0)
		}
		t0 = time.Now()
		if err := m.vm.Resume(); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		if err := m.waitState(vz.VirtualMachineStateRunning, 10*time.Second); err != nil {
			return err
		}
		tRes := time.Since(t0)
		if o.ip != "" {
			d, note, _, err := mcpPost(o.ip, toolsListBody, lastSession)
			if err != nil {
				return err
			}
			record("Resume() + tools/list", tRes+d, note, 0)
		} else {
			d, err := m.ping("pz", 10*time.Second)
			if err != nil {
				return err
			}
			record("Resume() + comando", tRes+d, "", 0)
		}
	}

	if *balloonB > 0 {
		t0 = time.Now()
		devs := m.vm.MemoryBalloonDevices()
		if len(devs) == 0 {
			return fmt.Errorf("sin dispositivo de globo")
		}
		vz.AsVirtioTraditionalMemoryBalloonDevice(devs[0]).SetTargetVirtualMachineMemorySize(uint64(*balloonB) << 20)
		time.Sleep(*idle)
		record(fmt.Sprintf("globo -> %d MiB antes de congelar", *balloonB), time.Since(t0), "", 0)
	}

	if *save != "" {
		t0 = time.Now()
		if err := m.vm.Pause(); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		if err := m.waitState(vz.VirtualMachineStatePaused, 10*time.Second); err != nil {
			return err
		}
		record("Pause()", time.Since(t0), "", 0)

		// El framework se niega a pisar un fichero existente.
		os.Remove(*save)
		t0 = time.Now()
		if err := m.vm.SaveMachineStateToPath(*save); err != nil {
			return fmt.Errorf("save: %w", err)
		}
		st, _ := os.Stat(*save)
		var size int64
		if st != nil {
			size = st.Size()
		}
		record("SaveMachineStateToPath (freeze)", time.Since(t0), "", size)
		// El overlay tiene que viajar con el estado: lo que el invitado
		// escribió antes de congelarse forma parte de la máquina.
		if out, err := exec.Command("cp", "-c", m.overlay, *save+".overlay.ext4").CombinedOutput(); err != nil {
			return fmt.Errorf("guardar overlay: %v: %s", err, out)
		}
	}

	t0 = time.Now()
	if err := m.vm.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	record("Stop()", time.Since(t0), "", 0)
	return dumpJSON(*jsonOut)
}

// ---------------------------------------------------------------- restore

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	o := commonFlags(fs)
	state := fs.String("state", "", "estado guardado por `boot -save`")
	n := fs.Int("n", 1, "cuántas máquinas restaurar del mismo estado (densidad)")
	idle := fs.Duration("idle", 2*time.Second, "reposo tras restaurar antes de medir memoria estable")
	balloon := fs.Int("balloon", 0, "tras restaurar, pedir al globo que deje al invitado en estos MiB (0 = no tocar)")
	keep := fs.Duration("keep", 0, "mantener las VMs vivas este tiempo antes de parar")
	jsonOut := fs.String("json", "", "escribir resultados en este fichero")
	parallel := fs.Bool("parallel", false, "restaurar las N máquinas a la vez en vez de una tras otra")
	gate := fs.Int("gate", 0, "con -parallel, restauraciones simultáneas como mucho (0 = sin límite)")
	stress := fs.Int("stress", 0, "antes del globo, que cada invitado ocupe y libere estos MiB en tmpfs")
	pulse := fs.Int("pulse", 0, "tras restaurar, inflar el globo hasta estos MiB y volver a soltarlo (descompromete las páginas libres que el restore dejó sucias)")
	release := fs.Int("release", 0, "hasta cuántos MiB soltar el globo tras el pulso (0 = toda la RAM configurada)")
	fs.Parse(args)
	if *state == "" {
		return fmt.Errorf("falta -state")
	}
	// Cada réplica parte del overlay tal y como quedó al congelar.
	if !o.noclone {
		o.overlay = *state + ".overlay.ext4"
	}
	o.idPath = *state + ".id"

	record("baseline (proceso vacío)", 0, "", 0)
	physBase, _ := mem()

	var ms_ []*machine
	tAll := time.Now()
	restoreOne := func(i int) (*machine, error) {
		t0 := time.Now()
		m, err := o.newMachine(i)
		if err != nil {
			return nil, err
		}
		tCfg := time.Since(t0)

		t0 = time.Now()
		if err := m.vm.RestoreMachineStateFromURL(*state); err != nil {
			return nil, fmt.Errorf("restore #%d: %w", i, err)
		}
		if err := m.waitState(vz.VirtualMachineStatePaused, 30*time.Second); err != nil {
			return nil, err
		}
		tRestore := time.Since(t0)

		t0 = time.Now()
		if err := m.vm.Resume(); err != nil {
			return nil, fmt.Errorf("resume #%d: %w", i, err)
		}
		if err := m.waitState(vz.VirtualMachineStateRunning, 10*time.Second); err != nil {
			return nil, err
		}
		tResume := time.Since(t0)

		record(fmt.Sprintf("#%d configurar", i), tCfg, "", 0)
		record(fmt.Sprintf("#%d Restore (thaw, carga estado)", i), tRestore, "", 0)
		record(fmt.Sprintf("#%d Resume (thaw, vCPU en marcha)", i), tResume, "", 0)
		if o.ip != "" && o.sameNet && i > 0 {
			// misma IP para todas: la NAT no distingue; solo se mide memoria
		} else if o.ip != "" {
			d, err := waitHealthz(m.ip, 30*time.Second)
			if err != nil {
				return nil, err
			}
			record(fmt.Sprintf("#%d puente responde tras thaw", i), d, "restore+resume+healthz = "+fmt.Sprintf("%.1f ms", ms(tRestore+tResume+d)), 0)
			sid, _ := os.ReadFile(*state + ".session")
			d, note, _, err := mcpPost(m.ip, toolsListBody, strings.TrimSpace(string(sid)))
			if err != nil {
				return nil, err
			}
			record(fmt.Sprintf("#%d tools/list tras thaw (sesión del snapshot)", i), d, note, 0)
		} else {
			d, err := m.ping(fmt.Sprintf("r%d", i), 15*time.Second)
			if err != nil {
				return nil, fmt.Errorf("máquina #%d no responde tras restaurar: %w", i, err)
			}
			record(fmt.Sprintf("#%d primer comando respondido", i), d, "restore+resume+cmd = "+fmt.Sprintf("%.1f ms", ms(tRestore+tResume+d)), 0)
		}
		return m, nil
	}
	if *parallel {
		type res struct {
			m   *machine
			err error
		}
		ch := make(chan res, *n)
		g := *gate
		if g <= 0 {
			g = *n
		}
		sem := make(chan struct{}, g)
		for i := 0; i < *n; i++ {
			go func(i int) {
				sem <- struct{}{}
				defer func() { <-sem }()
				m, err := restoreOne(i)
				ch <- res{m, err}
			}(i)
		}
		for i := 0; i < *n; i++ {
			r := <-ch
			if r.err != nil {
				return r.err
			}
			ms_ = append(ms_, r.m)
		}
	} else {
		for i := 0; i < *n; i++ {
			m, err := restoreOne(i)
			if err != nil {
				return err
			}
			ms_ = append(ms_, m)
		}
	}
	mode := "en serie"
	if *parallel {
		mode = fmt.Sprintf("en paralelo (gate %d)", *gate)
	}
	record(fmt.Sprintf("%d máquinas restauradas %s", *n, mode), time.Since(tAll), "", 0)

	if *stress > 0 {
		// Cada invitado llena tmpfs (RAM) y lo borra: el kernel invitado ya no
		// usa esas páginas, pero el anfitrión sigue teniéndolas asignadas hasta
		// que el globo se las pide. Es el escenario exacto de `kling squeeze`.
		t0 := time.Now()
		for i, m := range ms_ {
			for len(m.lines) > 0 {
				<-m.lines
			}
			fmt.Fprintf(m.toGuest, "dd if=/dev/zero of=/tmp/fill bs=1M count=%d 2>/dev/null; echo FILLED-%d\n", *stress, i)
		}
		for i, m := range ms_ {
			if _, err := m.waitLine(fmt.Sprintf("FILLED-%d", i), 60*time.Second); err != nil {
				return err
			}
		}
		time.Sleep(*idle)
		physS, _ := mem()
		record(fmt.Sprintf("con %d MiB ocupados en tmpfs por invitado", *stress), time.Since(t0), fmt.Sprintf("%.1f MiB por máquina sobre baseline", (physS-physBase)/float64(*n)), 0)
		t0 = time.Now()
		for i, m := range ms_ {
			fmt.Fprintf(m.toGuest, "rm /tmp/fill; echo FREED-%d\n", i)
		}
		for i, m := range ms_ {
			if _, err := m.waitLine(fmt.Sprintf("FREED-%d", i), 60*time.Second); err != nil {
				return err
			}
		}
		time.Sleep(*idle)
		physF, _ := mem()
		record("tras liberarlos en el invitado (sin globo)", time.Since(t0), fmt.Sprintf("%.1f MiB por máquina sobre baseline", (physF-physBase)/float64(*n)), 0)
	}

	time.Sleep(*idle)
	phys, _ := mem()
	record(fmt.Sprintf("estable tras %s", *idle), 0, fmt.Sprintf("%.1f MiB por máquina sobre baseline", (phys-physBase)/float64(*n)), 0)

	if *pulse > 0 {
		setAll := func(mib int) error {
			for _, m := range ms_ {
				devs := m.vm.MemoryBalloonDevices()
				if len(devs) == 0 {
					return fmt.Errorf("sin dispositivo de globo")
				}
				vz.AsVirtioTraditionalMemoryBalloonDevice(devs[0]).SetTargetVirtualMachineMemorySize(uint64(mib) << 20)
			}
			return nil
		}
		t0 := time.Now()
		if err := setAll(*pulse); err != nil {
			return err
		}
		time.Sleep(*idle)
		p1, _ := mem()
		record(fmt.Sprintf("pulso: globo -> %d MiB", *pulse), time.Since(t0), fmt.Sprintf("%.1f MiB por máquina sobre baseline", (p1-physBase)/float64(*n)), 0)
		rel := o.memMiB
		if *release > 0 {
			rel = *release
		}
		t0 = time.Now()
		if err := setAll(rel); err != nil {
			return err
		}
		time.Sleep(*idle)
		p2, _ := mem()
		record(fmt.Sprintf("pulso: globo -> %d MiB (soltado)", rel), time.Since(t0), fmt.Sprintf("%.1f MiB por máquina sobre baseline", (p2-physBase)/float64(*n)), 0)
		if o.ip != "" {
			sid, _ := os.ReadFile(*state + ".session")
			d, note, _, err := mcpPost(ms_[0].ip, toolsListBody, strings.TrimSpace(string(sid)))
			if err != nil {
				return fmt.Errorf("no responde tras el pulso: %w", err)
			}
			record("tools/list tras el pulso (#0)", d, note, 0)
		} else {
			d, err := ms_[0].ping("p0", 10*time.Second)
			if err != nil {
				return fmt.Errorf("no responde tras el pulso: %w", err)
			}
			record("comando tras el pulso (#0)", d, "", 0)
		}
	}

	if *balloon > 0 {
		t0 := time.Now()
		for _, m := range ms_ {
			devs := m.vm.MemoryBalloonDevices()
			if len(devs) == 0 {
				return fmt.Errorf("sin dispositivo de globo")
			}
			vz.AsVirtioTraditionalMemoryBalloonDevice(devs[0]).SetTargetVirtualMachineMemorySize(uint64(*balloon) << 20)
		}
		time.Sleep(*idle)
		phys2, _ := mem()
		record(fmt.Sprintf("globo -> %d MiB por máquina", *balloon), time.Since(t0), fmt.Sprintf("%.1f MiB por máquina sobre baseline", (phys2-physBase)/float64(*n)), 0)
		// ¿sigue respondiendo con menos memoria?
		for i, m := range ms_ {
			if o.ip != "" {
				sid, _ := os.ReadFile(*state + ".session")
				d, note, _, err := mcpPost(m.ip, toolsListBody, strings.TrimSpace(string(sid)))
				if err != nil {
					return fmt.Errorf("máquina #%d no responde tras el globo: %w", i, err)
				}
				if i == 0 {
					record("tools/list tras globo (#0)", d, note, 0)
				}
				if o.sameNet {
					break
				}
				continue
			}
			if d, err := m.ping(fmt.Sprintf("b%d", i), 10*time.Second); err != nil {
				return fmt.Errorf("máquina #%d no responde tras el globo: %w", i, err)
			} else if i == 0 {
				record("comando tras globo (#0)", d, "", 0)
			}
		}
	}

	if *keep > 0 {
		fmt.Printf("pid %d — %d VMs vivas durante %s\n", os.Getpid(), *n, *keep)
		time.Sleep(*keep)
	}

	t0 := time.Now()
	for _, m := range ms_ {
		if err := m.vm.Stop(); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
	}
	record(fmt.Sprintf("Stop() x%d", *n), time.Since(t0), "", 0)
	return dumpJSON(*jsonOut)
}

// cmdMaxVMs arranca máquinas mínimas una a una hasta que el framework se
// niegue, para ver si el tope es un número o la memoria: si cambia con -mem,
// es memoria.
func cmdMaxVMs(args []string) error {
	fs := flag.NewFlagSet("maxvms", flag.ExitOnError)
	o := commonFlags(fs)
	limit := fs.Int("limit", 64, "no pasar de aquí")
	fs.Parse(args)
	var ms_ []*machine
	defer func() {
		for _, m := range ms_ {
			m.vm.Stop()
		}
	}()
	for i := 0; i < *limit; i++ {
		m, err := o.newMachine(i)
		if err != nil {
			return err
		}
		if err := m.vm.Start(); err != nil {
			phys, _ := mem()
			fmt.Printf("FALLO al arrancar la máquina #%d (%d vivas) con mem=%d MiB: %v\n  phys total %.0f MiB · %s\n", i, len(ms_), o.memMiB, err, phys, vmFree())
			return nil
		}
		if err := m.waitState(vz.VirtualMachineStateRunning, 15*time.Second); err != nil {
			return err
		}
		if _, err := m.waitLine(o.ready, 30*time.Second); err != nil {
			return fmt.Errorf("#%d: %w", i, err)
		}
		ms_ = append(ms_, m)
		if (i+1)%4 == 0 {
			phys, _ := mem()
			fmt.Printf("  %2d vivas · phys %.0f MiB · %s\n", len(ms_), phys, vmFree())
		}
	}
	fmt.Printf("llegó a %d máquinas sin fallar (límite -limit)\n", len(ms_))
	return nil
}

// vmFree resume la memoria libre del sistema según vm_stat.
func vmFree() string {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return ""
	}
	var free, comp float64
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		v := strings.TrimSuffix(f[len(f)-1], ".")
		var n float64
		fmt.Sscanf(v, "%f", &n)
		switch {
		case strings.HasPrefix(l, "Pages free"):
			free = n * 16384 / (1 << 20)
		case strings.HasPrefix(l, "Pages occupied by compressor"):
			comp = n * 16384 / (1 << 20)
		}
	}
	return fmt.Sprintf("sistema: libres %.0f MiB, compresor %.0f MiB", free, comp)
}
