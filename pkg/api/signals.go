package api

// Las señales de parada de un servicio (ServiceSpec.StopSignal).
//
// La tabla vive aquí y no en el agente porque la usan los dos lados: el
// constructor "oci" comprueba el STOPSIGNAL de la imagen al construir (y corre
// también en macOS, donde los números de syscall no son los de Linux), y el
// agente la traduce al parar el servicio.

import (
	"fmt"
	"strconv"
	"strings"
)

// linuxSignals son las señales de Linux por nombre, sin "SIG". Los números
// son los de Linux en amd64 y arm64, que coinciden.
var linuxSignals = map[string]int{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "IOT": 6, "BUS": 7,
	"FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14,
	"TERM": 15, "STKFLT": 16, "CHLD": 17, "CLD": 17, "CONT": 18, "STOP": 19, "TSTP": 20,
	"TTIN": 21, "TTOU": 22, "URG": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27,
	"WINCH": 28, "IO": 29, "POLL": 29, "PWR": 30, "SYS": 31,
}

// Las de tiempo real, como las numera Docker: SIGRTMIN es 34 (glibc y musl se
// quedan 32 y 33), SIGRTMAX 64.
const (
	linuxSigRTMin = 34
	linuxSigRTMax = 64
)

// ParseSignal entiende un STOPSIGNAL de Docker: "SIGTERM", "term", "15",
// "SIGRTMIN+3", "RTMAX-1". Devuelve el número en Linux; "" es SIGTERM.
func ParseSignal(s string) (int, error) {
	if s == "" {
		return 15, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 || n > linuxSigRTMax {
			return 0, fmt.Errorf("signal %d out of range (1..%d)", n, linuxSigRTMax)
		}
		return n, nil
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if n, ok := linuxSignals[name]; ok {
		return n, nil
	}
	if rest, ok := strings.CutPrefix(name, "RTMIN"); ok {
		if off, ok := rtOffset(rest, "+"); ok && linuxSigRTMin+off <= linuxSigRTMax {
			return linuxSigRTMin + off, nil
		}
	}
	if rest, ok := strings.CutPrefix(name, "RTMAX"); ok {
		if off, ok := rtOffset(rest, "-"); ok && linuxSigRTMax-off >= linuxSigRTMin {
			return linuxSigRTMax - off, nil
		}
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

// rtOffset lee el "+n" de RTMIN+n (o el "-n" de RTMAX-n); "" es 0.
func rtOffset(rest, op string) (int, bool) {
	if rest == "" {
		return 0, true
	}
	num, ok := strings.CutPrefix(rest, op)
	if !ok || num == "" || len(num) > 2 {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 || num != strconv.Itoa(n) {
		return 0, false
	}
	return n, true
}
