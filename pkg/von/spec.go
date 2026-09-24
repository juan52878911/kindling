package von

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Spec es lo que el constructor "llm" recibe en el spec de POST /images. O un
// modelo del catálogo (Model + Quant), o un GGUF propio de Hugging Face fijado
// por revisión y hash (URL + SHA256).
type Spec struct {
	Model string `json:"model,omitempty"`
	Quant string `json:"quant,omitempty"`

	// URL es un GGUF de Hugging Face con la revisión fijada:
	// https://huggingface.co/<org>/<repo>/resolve/<commit de 40 hex>/<fichero>.gguf
	// Solo Hugging Face y solo por commit: una rama (main) dejaría de ser el
	// mismo fichero sin avisar, y el hash fallaría meses después.
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`

	// Ctx es el contexto TOTAL de llama-server (se reparte entre Parallel
	// ranuras). 0 = DefaultCtx.
	Ctx int `json:"ctx,omitempty"`
	// Parallel son las ranuras: peticiones que una réplica atiende a la vez.
	// 0 = 1. Escalar es mejor con más réplicas que con más ranuras: cada réplica
	// cuesta poco más que su caché KV, y no se pisan la CPU.
	Parallel int `json:"parallel,omitempty"`
	// Threads son los hilos de cálculo. 0 = uno por vCPU (nproc en el invitado).
	Threads int `json:"threads,omitempty"`
	// AcceptLicense es el identificador de licencia que quien construye acepta
	// para un modelo del catálogo que no es de licencia abierta (Model.Open).
	// Tiene que ser exactamente el suyo: aceptar "lo que sea" no es aceptar.
	AcceptLicense string `json:"accept_license,omitempty"`
	// Kind y Pooling hacen de un GGUF propio (URL) un codificador de frases:
	// Kind "embed" y Pooling mean, cls o last. Los del catálogo ya los traen.
	Kind    string `json:"kind,omitempty"`
	Pooling string `json:"pooling,omitempty"`
}

// Resolved es un Spec validado y completo: lo que el constructor necesita.
type Resolved struct {
	Ref      string // valor de la etiqueta von.model y alias en la API
	URL      string
	SHA256   string
	File     string // nombre del GGUF dentro de /models
	Size     int64  // 0 si no se conoce (GGUF propio)
	Ctx      int
	Parallel int
	Threads  int
	// Model es la entrada del catálogo, si viene de él.
	Model *Model
	// Kind y Pooling: ver Spec. Kind vacío = modelo instruct.
	Kind    string
	Pooling string
}

var (
	reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// https://huggingface.co/<org>/<repo>/resolve/<commit>/<fichero>.gguf, sin
	// subdirectorios: el nombre del fichero acaba en una ruta del invitado y en
	// un argumento de llama-server, y así no hay nada que escapar.
	reHFURL = regexp.MustCompile(`^https://huggingface\.co/([A-Za-z0-9][A-Za-z0-9._-]{0,95})/([A-Za-z0-9][A-Za-z0-9._-]{0,95})/resolve/([0-9a-f]{40})/([A-Za-z0-9][A-Za-z0-9._-]{0,127}\.gguf)$`)
)

// Resolve valida el spec y rellena los valores por defecto.
func (s Spec) Resolve() (Resolved, error) {
	var r Resolved
	switch {
	case s.Model != "" && s.URL != "":
		return r, fmt.Errorf("give a catalog model or a url, not both")
	case s.Model != "":
		if s.SHA256 != "" {
			return r, fmt.Errorf("sha256 only goes with url: catalog models carry their own")
		}
		m, err := Find(s.Model, s.Quant)
		if err != nil {
			return r, err
		}
		if !m.Open() && s.AcceptLicense != m.License {
			return r, fmt.Errorf("%s is not in the default catalog: its license is %q (%s), which does not allow free use and redistribution; "+
				"read it, and if it fits what you do, pass -accept-license %s", m.Ref(), m.License, m.LicenseURL, m.License)
		}
		r = Resolved{Ref: m.Ref(), URL: m.URL(), SHA256: m.SHA256, File: m.File, Size: m.Size, Model: &m}
	case s.URL != "":
		mm := reHFURL.FindStringSubmatch(s.URL)
		if mm == nil {
			return r, fmt.Errorf("url must be https://huggingface.co/<org>/<repo>/resolve/<40-hex commit>/<file>.gguf, got %q", s.URL)
		}
		if !reSHA256.MatchString(s.SHA256) {
			return r, fmt.Errorf("a url needs its sha256 (64 lowercase hex): the file is verified before it goes into the image")
		}
		if s.Quant != "" {
			return r, fmt.Errorf("quant only goes with a catalog model: a url already names one file")
		}
		file := mm[4]
		r = Resolved{Ref: strings.ToLower(strings.TrimSuffix(file, path.Ext(file))), URL: s.URL, SHA256: s.SHA256, File: file}
	default:
		return r, fmt.Errorf("missing model: a catalog id (%s) or a url", strings.Join(IDs(), ", "))
	}

	r.Ctx, r.Parallel, r.Threads = s.Ctx, s.Parallel, s.Threads
	if r.Ctx == 0 {
		r.Ctx = DefaultCtx
	}
	if r.Parallel == 0 {
		r.Parallel = 1
	}
	if err := resolveEmbed(&r, s); err != nil {
		return r, err
	}
	if r.Ctx < 256 || r.Ctx > 131072 {
		return r, fmt.Errorf("ctx out of range: %d (256..131072)", r.Ctx)
	}
	if r.Parallel < 1 || r.Parallel > 16 {
		return r, fmt.Errorf("parallel out of range: %d (1..16)", r.Parallel)
	}
	if r.Ctx/r.Parallel < 256 {
		return r, fmt.Errorf("ctx %d split over %d slots leaves under 256 tokens each", r.Ctx, r.Parallel)
	}
	if r.Threads < 0 || r.Threads > 64 {
		return r, fmt.Errorf("threads out of range: %d (0 = one per vCPU, up to 64)", r.Threads)
	}
	return r, nil
}

// ModelPath es la ruta del GGUF dentro del invitado.
func (r Resolved) ModelPath() string { return "/models/" + r.File }

// LayerMiB es el techo de la capa: el GGUF, llama.cpp (~35 MB desempaquetado),
// sus bibliotecas y el agente, con margen para las listas de apt mientras se
// instalan. Es un techo y no una reserva: la capa se encoge al terminar.
func (r Resolved) LayerMiB(modelBytes int64) int {
	return int(modelBytes>>20) + 512
}

// ServerArgs son los argumentos de llama-server. El número de hilos se deja
// como "$THREADS" para que lo resuelva el script de arranque (nproc).
//
// Cada uno está por algo:
//   - --host 0.0.0.0: el host llega por la red del invitado; fuera de ella no
//     hay nada (egress none y el proxy del daemon solo deja kling.ports).
//   - --no-webui: la interfaz web no sirve de nada detrás de una API y engorda
//     la memoria del dorado.
//   - --cache-ram 0: la caché de prompts en RAM viene a 8 GiB por defecto;
//     en una microVM de 768 MiB acabaría en el OOM killer al primer uso real.
//   - --fit off: con el contexto fijado, que llama.cpp no reajuste nada según
//     la memoria libre; el dorado tiene que ser el mismo en cada construcción.
//   - --metrics: /metrics de Prometheus, para que el gateway vea colas y ritmo.
//   - --load-mode dio: leer los pesos con O_DIRECT, sin mmap. Con el mmap por
//     defecto llama.cpp reempaqueta los pesos Q8_0 para la CPU (una copia en
//     memoria anónima) y la caché de páginas guarda OTRA del fichero: medido con
//     SmolLM2 en 768 MiB, 503 MiB anónimos + 197 MiB de caché y 32 MiB libres,
//     y un dorado de 648 MiB. Con O_DIRECT la única copia es la que se usa y el
//     dorado baja a 527 MiB.
//
// El sembrado aleatorio se queda en su valor por defecto (-1, nueva semilla por
// petición); docs/von.md explica por qué eso da semillas distintas en cada
// réplica restaurada del mismo dorado.
func (r Resolved) ServerArgs() []string {
	return []string{
		"--model", r.ModelPath(),
		"--alias", r.Ref,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(Port),
		"--ctx-size", strconv.Itoa(r.Ctx),
		"--parallel", strconv.Itoa(r.Parallel),
		"--threads", "$THREADS",
		"--no-webui",
		"--cache-ram", "0",
		"--fit", "off",
		"--metrics",
		"--load-mode", "dio",
	}
}

// RunScript es /etc/von/run.sh: arranca llama-server. Lo supervisa el
// entrypoint de la imagen (lo relanza si muere), así que termina en exec.
func (r Resolved) RunScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Generado por el constructor llm de kindling (kling models add).\n")
	b.WriteString("export LD_LIBRARY_PATH=/opt/llama.cpp\n")
	if r.Threads > 0 {
		fmt.Fprintf(&b, "THREADS=%d\n", r.Threads)
	} else {
		// Un hilo por vCPU: más hilos que vCPUs solo añaden cambios de contexto,
		// y el número de vCPUs queda fijado en el dorado.
		b.WriteString("THREADS=$(nproc)\n")
	}
	b.WriteString("exec /opt/llama.cpp/llama-server")
	for _, a := range append(r.ServerArgs(), r.EmbedArgs()...) {
		if a == "$THREADS" {
			b.WriteString(` "$THREADS"`)
			continue
		}
		// Todo lo demás sale de valores validados (sin comillas ni espacios),
		// pero se entrecomilla igual: el script corre como PID 1 del invitado.
		b.WriteString(" '" + strings.ReplaceAll(a, "'", `'\''`) + "'")
	}
	b.WriteString("\n")
	return b.String()
}
