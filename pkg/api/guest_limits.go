package api

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Los ulimits y sysctls del servicio (ServiceSpec.Ulimits y Sysctls).
//
// Una imagen de Docker trae lo que el motor le da por defecto (nofile
// 1048576, y el host pone vm.max_map_count): sin ello OpenSearch y ClickHouse
// no arrancan y Mongo o Redis avisan. El agente los aplica antes de lanzar el
// servicio. La microVM es el aislamiento: un sysctl cambia el kernel del
// invitado y de nadie más, pero solo se admiten los de una lista, no
// cualquiera (nada de kernel.core_pattern, modprobe ni binfmt_misc, que
// ejecutan cosas como root).

// UlimitUnlimited es el valor de "unlimited" (RLIM_INFINITY).
const UlimitUnlimited = math.MaxUint64

// Ulimit es un límite de recursos ya leído: Soft y Hard.
type Ulimit struct {
	Name       string
	Soft, Hard uint64
}

// UlimitNames son los recursos admitidos (los de setrlimit(2) de Linux).
var UlimitNames = []string{"core", "cpu", "data", "fsize", "locks", "memlock", "msgqueue",
	"nice", "nofile", "nproc", "rss", "rtprio", "sigpending", "stack", "as"}

// MaxUlimits y MaxSysctls acotan las listas del spec.
const (
	MaxUlimits = 16
	MaxSysctls = 32
)

func parseLimitValue(s string) (uint64, error) {
	if s == "unlimited" || s == "-1" {
		return UlimitUnlimited, nil
	}
	return strconv.ParseUint(s, 10, 63)
}

// ParseUlimit lee "nombre=blando[:duro]" como el --ulimit de Docker: el valor
// es un número, o unlimited (-1). Sin duro, el duro es el blando.
func ParseUlimit(s string) (Ulimit, error) {
	name, val, ok := strings.Cut(s, "=")
	if !ok || val == "" {
		return Ulimit{}, fmt.Errorf("ulimit %q: use name=soft[:hard] (nofile=65536)", s)
	}
	known := false
	for _, n := range UlimitNames {
		known = known || n == name
	}
	if !known {
		return Ulimit{}, fmt.Errorf("ulimit %q: unknown resource %q (want one of %s)", s, name, strings.Join(UlimitNames, ", "))
	}
	soft, hard, _ := strings.Cut(val, ":")
	u := Ulimit{Name: name}
	var err error
	if u.Soft, err = parseLimitValue(soft); err != nil {
		return Ulimit{}, fmt.Errorf("ulimit %q: bad soft limit %q (a number, or unlimited)", s, soft)
	}
	u.Hard = u.Soft
	if strings.Contains(val, ":") {
		if u.Hard, err = parseLimitValue(hard); err != nil {
			return Ulimit{}, fmt.Errorf("ulimit %q: bad hard limit %q (a number, or unlimited)", s, hard)
		}
	}
	if u.Soft > u.Hard {
		return Ulimit{}, fmt.Errorf("ulimit %q: the soft limit is above the hard one", s)
	}
	return u, nil
}

// ParseUlimits valida y lee la lista; un recurso repetido es un error.
func ParseUlimits(list []string) ([]Ulimit, error) {
	if len(list) > MaxUlimits {
		return nil, fmt.Errorf("too many ulimits (%d, max %d)", len(list), MaxUlimits)
	}
	seen := map[string]bool{}
	out := make([]Ulimit, 0, len(list))
	for _, s := range list {
		u, err := ParseUlimit(s)
		if err != nil {
			return nil, err
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("ulimit %q given twice", u.Name)
		}
		seen[u.Name] = true
		out = append(out, u)
	}
	return out, nil
}

// SysctlAllowed son los sysctls que se admiten tal cual; sysctlAllowedPrefix,
// las familias enteras. Todos son del kernel del invitado, sin efecto fuera
// de la microVM, y no ejecutan nada.
var SysctlAllowed = map[string]bool{
	"vm.max_map_count":                    true,
	"vm.overcommit_memory":                true,
	"vm.overcommit_ratio":                 true,
	"vm.swappiness":                       true,
	"vm.dirty_ratio":                      true,
	"vm.dirty_background_ratio":           true,
	"vm.min_free_kbytes":                  true,
	"vm.vfs_cache_pressure":               true,
	"fs.file-max":                         true,
	"fs.nr_open":                          true,
	"fs.aio-max-nr":                       true,
	"fs.inotify.max_user_watches":         true,
	"fs.inotify.max_user_instances":       true,
	"kernel.shmmax":                       true,
	"kernel.shmall":                       true,
	"kernel.shmmni":                       true,
	"kernel.sem":                          true,
	"kernel.msgmax":                       true,
	"kernel.msgmnb":                       true,
	"kernel.msgmni":                       true,
	"kernel.pid_max":                      true,
	"kernel.threads-max":                  true,
	"net.core.somaxconn":                  true,
	"net.core.netdev_max_backlog":         true,
	"net.core.rmem_max":                   true,
	"net.core.wmem_max":                   true,
	"net.core.rmem_default":               true,
	"net.core.wmem_default":               true,
	"net.core.optmem_max":                 true,
	"net.ipv4.ip_local_port_range":        true,
	"net.ipv4.ip_unprivileged_port_start": true,
	"net.ipv4.ping_group_range":           true,
}

// sysctlAllowedPrefix: net.ipv4.tcp_* (tcp_max_syn_backlog, tcp_tw_reuse,
// tcp_keepalive_time...).
var sysctlAllowedPrefix = []string{"net.ipv4.tcp_"}

var (
	reSysctlKey = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+){1,4}$`)
	reSysctlVal = regexp.MustCompile(`^[0-9A-Za-z_.: \t-]{1,256}$`)
)

// CheckSysctl valida un sysctl del spec: clave en notación de puntos, de la
// lista permitida, y un valor sin saltos de línea ni nada raro.
func CheckSysctl(k, v string) error {
	if !reSysctlKey.MatchString(k) {
		return fmt.Errorf("invalid sysctl name %q (use the dotted form, vm.max_map_count)", k)
	}
	ok := SysctlAllowed[k]
	for _, p := range sysctlAllowedPrefix {
		ok = ok || strings.HasPrefix(k, p)
	}
	if !ok {
		return fmt.Errorf("sysctl %q is not allowed (only %s and %s* are)", k, strings.Join(SysctlNames(), ", "), strings.Join(sysctlAllowedPrefix, ", "))
	}
	if !reSysctlVal.MatchString(v) {
		return fmt.Errorf("sysctl %s: invalid value %q", k, v)
	}
	return nil
}

// SysctlNames son los sysctls admitidos tal cual, ordenados.
func SysctlNames() []string {
	out := make([]string, 0, len(SysctlAllowed))
	for k := range SysctlAllowed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CheckSysctls valida el mapa del spec.
func CheckSysctls(m map[string]string) error {
	if len(m) > MaxSysctls {
		return fmt.Errorf("too many sysctls (%d, max %d)", len(m), MaxSysctls)
	}
	for k, v := range m {
		if err := CheckSysctl(k, v); err != nil {
			return err
		}
	}
	return nil
}

// ParseSysctlFlag lee "clave=valor" (la bandera -sysctl) y lo valida.
func ParseSysctlFlag(s string) (k, v string, err error) {
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		return "", "", fmt.Errorf("sysctl %q: use name=value (vm.max_map_count=262144)", s)
	}
	return k, v, CheckSysctl(k, v)
}
