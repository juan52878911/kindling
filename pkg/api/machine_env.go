package api

// El entorno de una máquina dado al arrancarla (RunRequest.Env).
//
// Hasta ahora el entorno de `kling run -image <ref> -e K=V` se horneaba en la
// imagen (/etc/kling/env): dos contraseñas eran dos imágenes enteras y el
// secreto quedaba en el disco de la imagen y en cada copia de ella. Ahora
// viaja así:
//
//	CLI ──(cuerpo de POST /machines)──▶ daemon ──(MMDS, antes de arrancar)──▶ kling-guest
//
// El daemon no lo guarda: ni en state.json ni en `kling inspect` (solo
// Machine.EnvKeys, los NOMBRES). Lo escribe en el almacén MMDS del VMM antes
// de InstanceStart, con MachineEnvBootParam=1 en la línea del kernel para que
// el agente sepa que tiene que leerlo, y lo borra del almacén en cuanto el
// agente contesta (para entonces ya lo leyó: lo lee antes de escuchar). El
// agente lo pone encima del entorno de la imagen (gana la máquina) para el
// servicio supervisado, la sonda de listo, los ganchos y exec; no lo escribe
// en ningún fichero.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// CapabilityMachineEnv es la capacidad de GET /info que dice que el daemon
// entiende RunRequest.Env. Un daemon anterior ignoraría el campo sin error y
// arrancaría la máquina sin su entorno: el CLI la mira antes de mandarlo.
const CapabilityMachineEnv = "machine-env"

// CapabilityStart es la capacidad de GET /info que dice que el daemon tiene
// POST /machines/{ref}/start (arrancar otra vez una máquina parada).
const CapabilityStart = "start"

// MachineEnvBootParam le dice al agente que hay entorno de la máquina en MMDS
// y que no arranque el servicio sin él.
const MachineEnvBootParam = "kling.env"

// MachineEnvMMDSKey es la clave del almacén MMDS que lo lleva. Aparte de
// "env" (el de las sesiones del puente y los marcadores de credenciales): no
// se mezcla con lo que esos caminos ponen y quitan.
const MachineEnvMMDSKey = "machine_env"

// Límites del entorno de una máquina. Caben de sobra en el almacén MMDS
// (MaxMMDSBytes) aun escapados en JSON y junto a los secretos de sesión y
// los marcadores de credenciales; el entorno no es sitio para ficheros.
const (
	MaxMachineEnvVars  = 256
	MaxMachineEnvBytes = 32 << 10
)

// MaxMMDSBytes es el tamaño máximo del almacén MMDS de una máquina, en JSON:
// el que acepta kling-vz y con el que el daemon lanza Firecracker
// (--mmds-size-limit; su defecto, 50 KiB, no daba para un entorno de 32 KiB
// más los secretos de sesión). Es también el tope del cuerpo de
// POST /machines/{ref}/mmds.
const MaxMMDSBytes = 1 << 20

var reMachineEnvKey = lazyre.New(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MachineEnvMap valida las entradas KEY=valor de RunRequest.Env y las
// devuelve como mapa (una clave repetida se queda con el último valor). Los
// errores nombran la clave, nunca el valor: puede ser una contraseña.
func MachineEnvMap(env []string) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	if len(env) > MaxMachineEnvVars {
		return nil, fmt.Errorf("too many environment variables (%d, max %d)", len(env), MaxMachineEnvVars)
	}
	out := make(map[string]string, len(env))
	total := 0
	for i, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !reMachineEnvKey.MatchString(k) {
			if !reMachineEnvKey.MatchString(k) {
				k = fmt.Sprintf("#%d", i+1)
			}
			return nil, fmt.Errorf("invalid environment entry %s: use KEY=value (KEY is letters, digits and _)", k)
		}
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("invalid environment entry %s: the value has a NUL byte", k)
		}
		total += len(kv) + 1
		out[k] = v
	}
	if total > MaxMachineEnvBytes {
		return nil, fmt.Errorf("the environment is %d bytes, over the limit of %d", total, MaxMachineEnvBytes)
	}
	return out, nil
}

// MachineEnvKeys son las claves de m, ordenadas: lo único del entorno que se
// guarda y se enseña.
func MachineEnvKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
