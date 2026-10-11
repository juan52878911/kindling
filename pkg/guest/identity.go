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
// Las claves de host SSH no se regeneran aquí: sshd las carga al arrancar y
// regenerarlas pide ssh-keygen dentro de la imagen y reiniciar sshd. kling save
// avisa de ellas (hostKeys) para que la imagen lo haga en un gancho.

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"strings"
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
