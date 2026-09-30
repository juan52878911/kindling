// Package android construye la imagen Android de kindling (Redroid 13 sobre
// una base Debian, con dm-verity) entera en Go: sin Docker, sin debootstrap,
// sin mkfs ni veritysetup, sin montar nada y sin ser root. Es el motor del
// constructor "android" del núcleo (`kling builder android`), y corre igual en
// un Mac que en el host Linux del daemon.
//
// Lo que sale es lo mismo que prototypes/android/image/build-image.sh con
// VERITY=1: una capa (Redroid en /android, el agente de invitado, el lanzador
// y lo que se pida) y una base glibc con el init que monta la capa a través de
// dm-verity. Todo lo que se descarga está fijado por sha256 (pins.go y
// debian_lock.go) y se comprueba antes de usarlo.
package android

// Redroid 13 "64only" (sin bibliotecas de 32 bits: Apple Silicon no ejecuta
// AArch32). Los digests son los de build-image.sh, leídos de Docker Hub el
// 2026-09-27: redroid/redroid:13.0.0_64only-240527 (publicada 2024-05-28).
// Se descarga el manifiesto de la arquitectura, no el índice: es el que manda.
type redroidPin struct {
	Repo, Tag, Index, Manifest, Layer string
}

var redroidPins = map[string]redroidPin{
	"arm64": {
		Repo: "redroid/redroid", Tag: "13.0.0_64only-240527",
		Index:    "sha256:5a42a569ee1d7c71796c0385e906cbaa4c3e0a162a56d9f26b29bdb1befac13b",
		Manifest: "sha256:c815ac1b1d5bd0a099b74c3e3e0eeea3b32bed8d7ab1f788c20fb74e35891716",
		Layer:    "sha256:7ae0e9111b77895cc932d126b75e9bf16af5f9bb881e3dae79163621e8b10cb3",
	},
	"amd64": {
		Repo: "redroid/redroid", Tag: "13.0.0_64only-240527",
		Index:    "sha256:5a42a569ee1d7c71796c0385e906cbaa4c3e0a162a56d9f26b29bdb1befac13b",
		Manifest: "sha256:36d6d21bcf7e92d78eabaa6f1748e5cf0e9fb15176c0091794168be012c5c22e",
		Layer:    "sha256:2824b019a4a8a038e79392f80302a3ddca69fdfdd6acb8c37b16e1461b2a6168",
	},
}

// debianBase es la base glibc: debian:trixie-slim por digest más los .deb
// fijados en debian_lock.go (internal/android/lockgen).
type debianBase struct {
	Image, Index, Manifest string
	// Snapshot es la marca de snapshot.debian.org del día en que se fijó: si
	// deb.debian.org ya no tiene un paquete, se busca ahí.
	Snapshot string
	Packages []debPin
}

type debPin struct {
	Name, Version string
	URL           string // deb.debian.org o security.debian.org + Filename
	SHA256        string
	Size          int64
}

// elfMachine es el e_machine de los binarios de cada arquitectura.
var elfMachine = map[string]uint16{"arm64": 0xb7, "amd64": 0x3e}

// libndkPin es de dónde sale libndk_translation con arm_translation
// "libndk": la imagen de sistema x86_64 del emulador de Android 14 (API 34,
// "Google APIs", revisión 14), tal como la sirve Google en su repositorio del
// SDK. La lista oficial
// (https://dl.google.com/android/repository/sys-img/google_apis/sys-img2-3.xml,
// "system-images;android-34;google_apis;x86_64") da el tamaño y el sha1; el
// sha256 se calculó el 2026-09-30 sobre el fichero bajado de esa URL, y el
// sha1 del mismo fichero coincide con el de la lista. Dentro, system.img es
// un disco GPT con una partición "super" (particiones dinámicas) y en ella el
// ext4 "system" con /system/lib64/libndk_translation.so (de julio de 2024,
// ro.ndk_translation.version=0.2.3), las bibliotecas arm64 que ve la app
// (/system/lib64/arm64) y el lanzador de binfmt_misc. No se redistribuye: lo
// baja quien construye (prototypes/android/docs/traduccion-arm.md).
//
// Se probó antes la de Android 12L (API 32, x86_64-32_r08.zip): su
// libndk_translation (2022) no tiene todos los "thunks" que usa el dpkg de
// Termux ("Bad thunk call") y el bootstrap no termina. La de API 33 no trae
// traducción.
var libndkPin = struct {
	URL, SHA256, SHA1 string
	Size              int64
	Entry, Partition  string
	Version, Android  string
}{
	URL:       "https://dl.google.com/android/repository/sys-img/google_apis/x86_64-34_r14.zip",
	SHA256:    "783a40134baf4f3012d4464fbe1571b1612a0dbd2e7a44d14bd8328923443833",
	SHA1:      "e0f6c9a0691aa27bd597d0deb1bcfdc943ac8ca7",
	Size:      1563721130,
	Entry:     "x86_64/system.img",
	Partition: "system",
	Version:   "0.2.3",
	Android:   "14 (API 34)",
}
