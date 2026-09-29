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
