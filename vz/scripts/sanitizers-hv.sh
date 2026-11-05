#!/bin/sh
# Las pruebas de C de kling-hv (vz/internal/hvvm) con AddressSanitizer y
# UndefinedBehaviorSanitizer: los dispositivos virtio contra un invitado
# hostil sobre la RAM de mentira de prueba.c, la validación de la cabecera del
# volcado, vq_cadena y vsock sin VM. No crean ninguna VM ni necesitan el
# entitlement de Hypervisor.framework (el binario de test no va firmado y lo
# que lo pediría da HV_DENIED, que es lo que prueba TestCreateSinEntitlement).
#
#   ./scripts/sanitizers-hv.sh                 todas las de internal/hvvm
#   ./scripts/sanitizers-hv.sh -run Colas      lo que vaya detrás va a go test
#   SANITIZE=undefined ./scripts/sanitizers-hv.sh   solo UBSan
#
# SANITIZE es la lista de -fsanitize (address,undefined por defecto). En
# macOS 26.5 el runtime de ASan (el de Apple clang 17 y el de LLVM 21) se
# cuelga al arrancar cualquier programa, uno de C de dos líneas incluido
# (InitializeShadowMemory vuelve a entrar en malloc desde dyld): ahí solo vale
# SANITIZE=undefined. El CI lo corre en macos-15 con los dos y un tope de
# tiempo, por si allí pasara lo mismo.
#
# Sin -race: el detector de carreras de Go y ASan no conviven en un binario.
# Un fallo de ASan o UBSan aborta el proceso del test (-fno-sanitize-recover),
# así que sale distinto de cero y con el informe en la salida.
set -eu
cd "$(dirname "$0")/.."
[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] || {
	echo "sanitizers-hv.sh: kling-hv is macOS on Apple Silicon only" >&2
	exit 2
}
san="-fsanitize=${SANITIZE:-address,undefined}"
CGO_ENABLED=1
CGO_CFLAGS="-O1 -g -fno-omit-frame-pointer $san -fno-sanitize-recover=undefined -mmacosx-version-min=15.2"
CGO_LDFLAGS="$san -mmacosx-version-min=15.2"
# detect_leaks no existe en macOS; abort_on_error para que un fallo pare el
# test aquí y no en un sitio cualquiera.
ASAN_OPTIONS="${ASAN_OPTIONS:-abort_on_error=1:halt_on_error=1}"
UBSAN_OPTIONS="${UBSAN_OPTIONS:-print_stacktrace=1:halt_on_error=1}"
export CGO_ENABLED CGO_CFLAGS CGO_LDFLAGS ASAN_OPTIONS UBSAN_OPTIONS
exec go test -count=1 "$@" ./internal/hvvm/
