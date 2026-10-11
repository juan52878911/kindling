package guest

// Rodeos del agente para CPU que el kernel del invitado describe mal.
//
// SVE2 SIN SVE (issue #183). En un Mac con M4 (SME, que trae SVE solo en modo
// "streaming"), el kernel 6.1 del invitado anuncia en sus hwcaps sve2,
// svei8mm y svebf16 sin sve: los saca de ID_AA64ZFR0_EL1 sin mirar si SVE
// existe (upstream lo arregló en 6.10, «arm64: Filter out SVE hwcaps when
// FEAT_SVE isn't implemented», y no llegó a 6.1.y). OpenSSL ve HWCAP2_SVE2,
// ejecuta sus rutas SVE2 y el proceso muere con SIGILL: medido con la rueda
// de cryptography de pip en python:3.12-slim. El arreglo de verdad es el
// kernel; hasta entonces el agente da a sus servicios y a exec
// OPENSSL_armcap=0x7d (NEON, AES, SHA-1, SHA-256, PMULL y SHA-512: las
// aceleraciones que sí hay, sin SVE), salvo que la imagen ya ponga uno.

import (
	"bufio"
	"log"
	"os"
	"runtime"
	"strings"
)

// Variables para los tests.
var (
	cpuinfoPath = "/proc/cpuinfo"
	goarch      = runtime.GOARCH
)

// opensslArmcapSinSVE es el OPENSSL_armcap que se da cuando el kernel anuncia
// SVE2 sin SVE.
const opensslArmcapSinSVE = "0x7d"

// cpuQuirkEnv añade a env lo que haga falta para la CPU de este invitado.
func cpuQuirkEnv(env []string) []string {
	if goarch != "arm64" {
		return env
	}
	f := cpuFeatures(cpuinfoPath)
	if !f["sve2"] || f["sve"] {
		return env
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENSSL_armcap=") {
			return env
		}
	}
	log.Printf("cpu: the kernel advertises sve2 without sve (Apple M4 under vz); OpenSSL gets OPENSSL_armcap=%s so it doesn't run SVE2 code that would die with SIGILL", opensslArmcapSinSVE)
	return append(env, "OPENSSL_armcap="+opensslArmcapSinSVE)
}

// cpuFeatures son las palabras de la primera línea Features de cpuinfo.
func cpuFeatures(p string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(p)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(k) != "Features" {
			continue
		}
		for _, w := range strings.Fields(v) {
			out[w] = true
		}
		break
	}
	return out
}
