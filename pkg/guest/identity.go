package guest

// Identidad por copia (issue #176).
//
// Todas las copias de un dorado traen el /etc/machine-id que el dorado tenía:
// systemd, journald, dbus y lo que derive un id estable de él (licencias,
// métricas, un DHCP client id) ven la misma máquina N veces. En cada
// restauración "instance" el agente escribe uno nuevo, ya con el CSPRNG del
// kernel resembrado por /resync.
//
// Solo se reescribe un machine-id que ya tiene valor: uno vacío significa
// "genéralo al arrancar" (systemd-firstboot) y uno ausente, que la imagen no
// lo usa. Un enlace simbólico (/var/lib/dbus/machine-id → /etc/machine-id) no
// se toca: ya sigue al otro.
//
// Las claves de host SSH se regeneran en la tanda de ganchos (sshRenewal),
// porque tarda; si la imagen no trae ssh-keygen, kling save avisa de ellas.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// machineIDFiles son los machine-id que se renuevan (variable para los tests).
var machineIDFiles = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// sshHostKeyGlob son las claves privadas de host de sshd (variable para los
// tests).
var sshHostKeyGlob = "/etc/ssh/ssh_host_*_key"

// renewMachineID escribe un machine-id nuevo, el mismo en todos los ficheros
// de paths que ya tenían uno, y devuelve los que cambió.
func renewMachineID(paths []string) []string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Printf("resync: machine-id: %v", err)
		return nil
	}
	id := hex.EncodeToString(b[:]) + "\n"
	var out []string
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		old, err := os.ReadFile(p)
		if err != nil || !esMachineID(strings.TrimSpace(string(old))) {
			continue
		}
		tmp := filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".kling")
		if err := os.WriteFile(tmp, []byte(id), fi.Mode().Perm()); err != nil {
			log.Printf("resync: machine-id %s: %v", p, err)
			continue
		}
		if err := os.Rename(tmp, p); err != nil {
			os.Remove(tmp)
			log.Printf("resync: machine-id %s: %v", p, err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// esMachineID dice si s es un machine-id con valor: 32 dígitos hexadecimales.
func esMachineID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// hostKeys son las claves privadas de host SSH que trae el invitado.
func hostKeys() []string {
	m, _ := filepath.Glob(sshHostKeyGlob)
	return m
}

// sshKeygenPaths son los sitios donde se busca ssh-keygen en el invitado
// (variable para los tests).
var sshKeygenPaths = []string{"/usr/bin/ssh-keygen", "/usr/local/bin/ssh-keygen", "/bin/ssh-keygen"}

// sshKeygen es el ssh-keygen de la imagen, o "" si no trae.
func sshKeygen() string {
	for _, p := range sshKeygenPaths {
		if isExec(p) == nil {
			return p
		}
	}
	return ""
}

// sshRenewal es lo que hay que hacer con las claves de host SSH en una copia,
// o nil si nada: la imagen no tiene claves o no tiene con qué regenerarlas
// (entonces `kling save` avisa, ver ProcsHandler).
//
// Cada clave se regenera con su tipo (sale del nombre: ssh_host_<tipo>_key),
// sin añadir tipos que la imagen no tenía. Después, SIGHUP al sshd que escucha:
// desde OpenSSH 9.8 el proceso que escucha carga las claves al arrancar y no
// las vuelve a leer hasta un HUP (medido: OpenSSH 10.0 seguía anunciando la
// clave vieja; 9.7, que vuelve a ejecutarse en cada conexión, cogía la nueva).
// Corre en la tanda de ganchos, no en /resync: una RSA de 3072 bits tarda.
func sshRenewal() func() error {
	keys, gen := hostKeys(), sshKeygen()
	if len(keys) == 0 || gen == "" {
		return nil
	}
	return func() error {
		for _, k := range keys {
			tipo := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(k), "ssh_host_"), "_key")
			if tipo == "" || strings.ContainsAny(tipo, "/ ") {
				continue
			}
			os.Remove(k)
			os.Remove(k + ".pub")
			ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
			out, err := runArgs(ctx, gen, "-q", "-t", tipo, "-N", "", "-C", "", "-f", k)
			cancel()
			if err != nil {
				return fmt.Errorf("regenerating %s: %s", k, conSalida(err, out))
			}
		}
		for _, pid := range sshdListeners(procRoot) {
			_ = syscall.Kill(pid, syscall.SIGHUP)
		}
		log.Printf("post-restore: %d SSH host key(s) regenerated", len(keys))
		return nil
	}
}

// sshdListeners son los sshd que escuchan: los que se llaman sshd y cuyo padre
// no es otro sshd (las sesiones son hijas suyas, y un HUP las cortaría).
func sshdListeners(root string) []int {
	ents, _ := os.ReadDir(root)
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		st, ok := readProcStat(root, pid)
		if !ok || st.comm != "sshd" {
			continue
		}
		if padre, ok := readProcStat(root, st.ppid); ok && strings.HasPrefix(padre.comm, "sshd") {
			continue
		}
		out = append(out, pid)
	}
	return out
}
