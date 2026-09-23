// Package von sirve modelos de lenguaje pequeños (VON) desde microVMs de
// kindling: llama-server de llama.cpp dentro de una imagen glibc, con los pesos
// GGUF en la capa de la imagen, congelado en un snapshot dorado DESPUÉS de
// cargar el modelo y calentarlo.
//
// La razón de hacerlo así, y no arrancar llama-server en frío: en Firecracker la
// memoria de un dorado se mapea perezosamente desde su mem.file, así que una
// réplica restaurada contesta en cuanto termina el thaw, con el modelo ya en
// memoria, y N réplicas del mismo dorado comparten en la caché de páginas del
// host las páginas de los pesos que ninguna ha escrito. Cifras y método en
// docs/von.md.
//
// El paquete no tiene dependencias fuera de la biblioteca estándar y de pkg/api:
// lo usan el CLI (`kling models`), el constructor "llm" que corre como root en
// el host del daemon, y lo usará el gateway que encadene JEV → VON.
package von

import (
	"fmt"
	"sort"
	"strings"
)

// Port es el puerto del invitado en el que escucha llama-server: su API
// compatible con OpenAI (/v1/chat/completions, /v1/models) y /health. No es el
// 8080 porque ahí está el agente de invitado (kling-guest), que sigue presente
// para exec y ficheros.
const Port = 8000

// LabelModel es la etiqueta que marca una máquina, y el snapshot que sale de
// ella, como un modelo VON. Su valor es el ID de catálogo con la cuantización
// (smollm2-360m-instruct:q8_0). Un gateway descubre los modelos servibles
// listando snapshots con esta etiqueta, sin mirar nombres.
const LabelModel = "von.model"

// DefaultCtx es el contexto por defecto. Pequeño a propósito: la caché KV crece
// lineal con él y se reserva entera al arrancar, así que es memoria de CADA
// réplica y del dorado. Estos modelos no aprovechan contextos largos.
const DefaultCtx = 2048

// Model es un GGUF fijado: repositorio, revisión (commit de Hugging Face) y
// sha256 del fichero. Sin revisión y hash, "el mismo modelo" podría ser otro el
// día que el autor suba una versión nueva, y el dorado dejaría de ser
// reproducible sin que nada lo avisara.
type Model struct {
	ID       string // smollm2-360m-instruct
	Quant    string // q8_0
	Repo     string // HuggingFaceTB/SmolLM2-360M-Instruct-GGUF
	Revision string // commit de 40 hex
	File     string // smollm2-360m-instruct-q8_0.gguf
	SHA256   string
	Size     int64 // bytes del GGUF

	// MemMiB y VCPUs son el tamaño por defecto de la microVM. La memoria sale de
	// medir (docs/von.md): pesos + caché KV de DefaultCtx + búfer de cálculo +
	// kernel y agente, con margen. Se puede cambiar con -mem.
	MemMiB  int
	VCPUs   int
	License string
}

// Ref es el nombre completo del modelo en una etiqueta: ID:cuantización.
func (m Model) Ref() string { return m.ID + ":" + m.Quant }

// URL es la descarga fijada a la revisión, no a main.
func (m Model) URL() string {
	return "https://huggingface.co/" + m.Repo + "/resolve/" + m.Revision + "/" + m.File
}

// Catalog son los modelos que kindling sabe construir con una sola orden. Los
// GGUF oficiales cuando el autor los publica; si no, los de bartowski, que son
// la referencia de facto.
var Catalog = []Model{
	{
		ID: "smollm2-360m-instruct", Quant: "q8_0",
		Repo:     "HuggingFaceTB/SmolLM2-360M-Instruct-GGUF",
		Revision: "593b5a2e04c8f3e4ee880263f93e0bd2901ad47f",
		File:     "smollm2-360m-instruct-q8_0.gguf",
		SHA256:   "48ab3034d0dd401fbc721eb1df3217902fee7dab9078992d66431f09b7750201",
		Size:     386404992, MemMiB: 768, VCPUs: 2, License: "apache-2.0",
	},
	{
		ID: "smollm2-360m-instruct", Quant: "q4_k_m",
		Repo:     "bartowski/SmolLM2-360M-Instruct-GGUF",
		Revision: "7be6f65f1db715fe5dc5a4634c0d459b4eed42ec",
		File:     "SmolLM2-360M-Instruct-Q4_K_M.gguf",
		SHA256:   "2fa3f013dcdd7b99f9b237717fa0b12d75bbb89984cc1274be1471a465bac9c2",
		Size:     270590880, MemMiB: 640, VCPUs: 2, License: "apache-2.0",
	},
	{
		ID: "qwen2.5-0.5b-instruct", Quant: "q4_k_m",
		Repo:     "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
		Revision: "9217f5db79a29953eb74d5343926648285ec7e67",
		File:     "qwen2.5-0.5b-instruct-q4_k_m.gguf",
		SHA256:   "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db",
		Size:     491400032, MemMiB: 896, VCPUs: 2, License: "apache-2.0",
	},
	{
		ID: "qwen2.5-0.5b-instruct", Quant: "q8_0",
		Repo:     "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
		Revision: "9217f5db79a29953eb74d5343926648285ec7e67",
		File:     "qwen2.5-0.5b-instruct-q8_0.gguf",
		SHA256:   "ca59ca7f13d0e15a8cfa77bd17e65d24f6844b554a7b6c12e07a5f89ff76844e",
		Size:     675710816, MemMiB: 1024, VCPUs: 2, License: "apache-2.0",
	},
}

// defaultQuant es la cuantización que se usa si no se pide ninguna: Q8_0, que
// en modelos de este tamaño apenas pierde calidad frente a los pesos originales.
const defaultQuant = "q8_0"

// Find busca un modelo del catálogo. quant vacío = la cuantización por defecto.
// Acepta el ID con la cuantización pegada (smollm2-360m-instruct:q8_0).
func Find(id, quant string) (Model, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if i, q, ok := strings.Cut(id, ":"); ok {
		if quant != "" && !strings.EqualFold(quant, q) {
			return Model{}, fmt.Errorf("model %q and quant %q disagree", id, quant)
		}
		id, quant = i, q
	}
	quant = strings.ToLower(strings.TrimSpace(quant))
	if quant == "" {
		quant = defaultQuant
	}
	var quants []string
	for _, m := range Catalog {
		if m.ID != id {
			continue
		}
		if m.Quant == quant {
			return m, nil
		}
		quants = append(quants, m.Quant)
	}
	if len(quants) > 0 {
		sort.Strings(quants)
		return Model{}, fmt.Errorf("model %s has no %s build; available: %s", id, quant, strings.Join(quants, ", "))
	}
	return Model{}, fmt.Errorf("unknown model %q; available: %s", id, strings.Join(IDs(), ", "))
}

// IDs son los modelos del catálogo, sin repetir y en el orden del catálogo.
func IDs() []string {
	var out []string
	visto := map[string]bool{}
	for _, m := range Catalog {
		if !visto[m.ID] {
			visto[m.ID] = true
			out = append(out, m.ID)
		}
	}
	return out
}

// LLAMA.CPP FIJADO.
//
// Se usan los binarios oficiales de la versión, no una compilación propia:
// compilar llama.cpp con todas las variantes de CPU tarda más que el plazo de
// un constructor (15 min) en un host modesto, y los oficiales ya traen
// GGML_CPU_ALL_VARIANTS —una biblioteca por nivel de instrucciones (armv8.0,
// 8.2 con dotprod, 8.6 con i8mm, 9.2 con SVE2; en x86, de SSE4.2 a AVX-512) que
// se elige al arrancar según la CPU—, que es justo la base portable que haría
// falta compilar a mano. Se verifica el sha256 que publica GitHub del tarball.
//
// Exigen glibc 2.38 (se compilan en Ubuntu 24.04): por eso la base es Debian
// trixie (glibc 2.41) y no la bookworm de 71-build-glibc-base.sh (2.36).
const LlamaTag = "b11147"

// LlamaAsset es el tarball oficial de una arquitectura.
type LlamaAsset struct {
	File   string
	SHA256 string
}

// LlamaAssets por GOARCH del host del daemon (el constructor corre ahí, y la
// microVM tiene su misma arquitectura).
var LlamaAssets = map[string]LlamaAsset{
	"arm64": {File: "llama-" + LlamaTag + "-bin-ubuntu-arm64.tar.gz",
		SHA256: "ff574b9e0a0ff26b7ad12a7e7534886ba03c80dd5de12a3244be323487afbe5d"},
	"amd64": {File: "llama-" + LlamaTag + "-bin-ubuntu-x64.tar.gz",
		SHA256: "e595f4cf393b3c9846a19bea6cf4bc24ae97982aecd7042de92de99c0ed320bd"},
}

// LlamaURL es la descarga del tarball de una arquitectura.
func LlamaURL(a LlamaAsset) string {
	return "https://github.com/ggml-org/llama.cpp/releases/download/" + LlamaTag + "/" + a.File
}

// BaseImage es la imagen base glibc sobre la que va la capa del modelo. La
// construye el propio constructor la primera vez (71-build-glibc-base.sh con
// SUITE=trixie), y la comparten todos los modelos.
const BaseImage = "glibc-trixie"

// BasePackages son las bibliotecas que llama-server necesita y la base mínima
// no trae: OpenMP (libgomp1) y OpenSSL 3, que enlaza libllama-common para
// descargar modelos —aquí no se usa, pero el enlazador dinámico lo exige—.
var BasePackages = []string{"libgomp1", "libssl3t64"}
