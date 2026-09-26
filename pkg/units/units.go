// Package units lee cantidades con sufijo en los flags del CLI: -mem 2G,
// -ttl 10m, -size 512M. Un entero desnudo vale lo que valía antes de que
// hubiera sufijos (MiB para memoria, segundos para tiempos), así que ningún
// script cambia de significado.
package units

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseMiB lee "512", "512M", "2G", "1024MiB", "0.5G" y devuelve MiB.
func ParseMiB(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("negative size %q", s)
		}
		return n, nil
	}
	num, unit := splitUnit(s)
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q: use 512, 512M or 2G", s)
	}
	var mib float64
	switch strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(unit), "b"), "i") {
	case "k":
		mib = f / 1024
	case "m", "":
		mib = f
	case "g":
		mib = f * 1024
	case "t":
		mib = f * 1024 * 1024
	default:
		return 0, fmt.Errorf("invalid size %q: use 512, 512M or 2G", s)
	}
	return int(mib + 0.5), nil
}

// ParseDuration lee "600" (segundos), "10m", "1h30m".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("negative duration %q", s)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q: use 600, 10m or 1h", s)
	}
	return d, nil
}

func splitUnit(s string) (num, unit string) {
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if c >= '0' && c <= '9' || c == '.' {
			break
		}
		i--
	}
	return s[:i], s[i:]
}

// mibValue es el flag.Value de una memoria en MiB.
type mibValue struct{ p *int }

func (v mibValue) String() string {
	if v.p == nil {
		return "0"
	}
	return strconv.Itoa(*v.p)
}

func (v mibValue) Set(s string) error {
	n, err := ParseMiB(s)
	if err != nil {
		return err
	}
	*v.p = n
	return nil
}

// MiBVar registra un flag de memoria que acepta sufijos y devuelve los MiB.
func MiBVar(fs *flag.FlagSet, name string, def int, usage string) *int {
	p := new(int)
	*p = def
	fs.Var(mibValue{p}, name, usage)
	return p
}

// durationValue es el flag.Value de un tiempo que acepta segundos desnudos.
type durationValue struct{ p *time.Duration }

func (v durationValue) String() string {
	if v.p == nil {
		return "0s"
	}
	return v.p.String()
}

func (v durationValue) Set(s string) error {
	d, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*v.p = d
	return nil
}

// DurationVar registra un flag de tiempo: "10m" o "600" (segundos).
func DurationVar(fs *flag.FlagSet, name string, def time.Duration, usage string) *time.Duration {
	p := new(time.Duration)
	*p = def
	fs.Var(durationValue{p}, name, usage)
	return p
}

// secondsValue es el flag.Value de un tiempo que se guarda en segundos.
type secondsValue struct{ p *int }

func (v secondsValue) String() string {
	if v.p == nil {
		return "0"
	}
	return strconv.Itoa(*v.p)
}

func (v secondsValue) Set(s string) error {
	d, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*v.p = int(d / time.Second)
	return nil
}

// SecondsVar registra un flag de tiempo que devuelve segundos enteros.
func SecondsVar(fs *flag.FlagSet, name string, def int, usage string) *int {
	p := new(int)
	*p = def
	fs.Var(secondsValue{p}, name, usage)
	return p
}
