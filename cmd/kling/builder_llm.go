package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/von"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/lazyre"
)

// CONSTRUCTOR "llm": una imagen VON (llama-server + un GGUF) como capa sobre
// una base glibc. Lo ejecuta el daemon como root en su host:
//
//	kling builder llm <directorio de trabajo>
//
// Lo que se descarga (llama.cpp y el GGUF) se hace aquí, en Go, y no en el
// script: así cada byte pasa por el sha256 antes de tocar la imagen, y una
// descarga cortada o manipulada no llega a construirse. Las descargas quedan
// en $KLING_ROOT/cache/von, por hash: el segundo modelo, o reconstruir el
// mismo, no vuelve a bajar nada.
//
// El montaje de la capa es el del constructor base (81-base-image.sh), con dos
// añadidos genéricos: ROOTFS_DIR (ficheros que copiar dentro) y SERVICE (un
// ejecutable que el entrypoint arranca y relanza antes de ceder al agente).

// llamaKeep dice qué ficheros del tarball de llama.cpp van a la imagen:
// llama-server, las bibliotecas que enlaza y TODAS las variantes de CPU de ggml,
// que se eligen al arrancar según la CPU. Lo demás (otras herramientas, el
// backend RPC) no se usa y engordaría la capa.
var llamaKeep = lazyre.New(`^(llama-server|LICENSE|libllama-server-impl\.so|libllama-common\.so.*|libmtmd\.so.*|libllama\.so.*|libggml\.so.*|libggml-base\.so.*|libggml-cpu-[A-Za-z0-9._-]+\.so)$`)

func builderLLM(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return err
	}
	var req api.BuildImageRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return fmt.Errorf("request.json: %w", err)
	}
	var spec von.Spec
	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &spec); err != nil {
			return fmt.Errorf("spec: %w", err)
		}
	}
	res, err := spec.Resolve()
	if err != nil {
		return err
	}
	// La base tiene que venir en la petición: el daemon la apunta en la receta,
	// y una receta sin base se lee como "min" (Alpine), donde llama-server no
	// arranca. Mejor fallar aquí que construir una imagen que no bootea.
	if req.Base == "" {
		return fmt.Errorf("the llm builder needs an explicit base (use %q)", von.BaseImage)
	}
	if err := validateBase(req, BaseSpec{}); err != nil {
		return err
	}
	asset, ok := von.LlamaAssets[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no llama.cpp build for %s", runtime.GOARCH)
	}

	root := envOr("KLING_ROOT", "/var/lib/kindling")
	lib := envOr("KLING_LIB_DIR", "/usr/local/lib/kindling")
	agent := envOr("KLING_GUEST_AGENT", filepath.Join(lib, "kling-guest"))
	script := envOr("KLING_BASE_IMAGE_SCRIPT", filepath.Join(lib, "81-base-image.sh"))
	if _, err := os.Stat(agent); err != nil {
		return fmt.Errorf("missing %s (installed by `make deploy`)", agent)
	}

	// 1. La base glibc, si no está. Es de todos los modelos y se hace una vez.
	pkgs := von.BasePackages
	if _, err := os.Stat(filepath.Join(root, "images", req.Base+".ext4")); err != nil {
		if req.Base != von.BaseImage {
			return fmt.Errorf("base image %q does not exist", req.Base)
		}
		if err := buildLLMBase(root, lib, req.Base); err != nil {
			return err
		}
	}
	if req.Base == von.BaseImage {
		// La base propia ya lleva las bibliotecas: sin paquetes, la capa se
		// construye sin red ni apt-get update.
		pkgs = nil
	}

	cache := filepath.Join(root, "cache", "von")
	if err := os.MkdirAll(filepath.Join(cache, "models"), 0o700); err != nil {
		return err
	}

	// 2. llama.cpp, verificado, y solo lo que hace falta.
	tarball := filepath.Join(cache, asset.File)
	if err := fetchVerified(von.LlamaURL(asset), asset.SHA256, tarball); err != nil {
		return fmt.Errorf("llama.cpp %s: %w", von.LlamaTag, err)
	}
	rootfs := filepath.Join(dir, "rootfs")
	optDir := filepath.Join(rootfs, "opt", "llama.cpp")
	if err := os.MkdirAll(optDir, 0o755); err != nil {
		return err
	}
	n, err := extractLlama(tarball, optDir)
	if err != nil {
		return fmt.Errorf("unpacking llama.cpp: %w", err)
	}
	if _, err := os.Stat(filepath.Join(optDir, "llama-server")); err != nil {
		return fmt.Errorf("the llama.cpp %s tarball has no llama-server", von.LlamaTag)
	}
	fmt.Printf("llama.cpp %s (%s): %d files\n", von.LlamaTag, runtime.GOARCH, n)

	// 3. El GGUF, verificado. En la caché va por hash: el nombre del fichero no
	// identifica nada (dos repos pueden llamar igual a dos ficheros distintos).
	cached := filepath.Join(cache, "models", res.SHA256+".gguf")
	if res.URL == "" {
		// Un GGUF que convierte kindling (los codificadores): no hay nada que
		// descargar, tiene que estar ya en la caché, y con su hash.
		if got, err := sha256File(cached); err != nil || got != res.SHA256 {
			// URL vacía solo sale de una entrada del catálogo convertida: Model
			// no es nil (Spec.Resolve exige URL a un GGUF propio).
			m := res.Model
			return fmt.Errorf("model %s is converted from %s by kindling, not downloaded: run\n"+
				"  sudo scripts/encoder-gguf.sh %s -install\n"+
				"on this host (it converts the pinned weights with llama.cpp %s's converter and checks sha256 %s)",
				res.Ref, m.Source+"@"+m.SourceRevision[:12], m.ID, von.LlamaTag, res.SHA256[:12])
		}
	} else if err := fetchVerified(res.URL, res.SHA256, cached); err != nil {
		return fmt.Errorf("model %s: %w", res.Ref, err)
	}
	st, err := os.Stat(cached)
	if err != nil {
		return err
	}
	modelDst := filepath.Join(rootfs, "models", res.File)
	if err := os.MkdirAll(filepath.Dir(modelDst), 0o755); err != nil {
		return err
	}
	// Enlace duro: la caché y el directorio de trabajo están en el mismo disco,
	// y copiar 700 MB para copiarlos otra vez dentro de la capa es tiempo tirado.
	if err := os.Link(cached, modelDst); err != nil {
		if err := copyFile(cached, modelDst); err != nil {
			return err
		}
	}
	_ = os.Chmod(modelDst, 0o644)
	fmt.Printf("model %s: %s (%d MiB, sha256 %s…)\n", res.Ref, res.File, st.Size()>>20, res.SHA256[:12])

	// 4. Cómo arrancarlo y qué es.
	etc := filepath.Join(rootfs, "etc", "von")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(etc, "run.sh"), []byte(res.RunScript()), 0o755); err != nil {
		return err
	}
	manifest, _ := json.MarshalIndent(map[string]any{
		"model": res.Ref, "file": res.File, "sha256": res.SHA256, "url": res.URL,
		"ctx": res.Ctx, "parallel": res.Parallel, "threads": res.Threads,
		"port": von.Port, "llama_cpp": von.LlamaTag, "api": "openai",
		"kind": res.Kind, "pooling": res.Pooling,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(etc, "model.json"), append(manifest, '\n'), 0o644); err != nil {
		return err
	}

	// 5. La capa. Sin paquetes que instalar (la base propia), en Go: una
	// escritura del GGUF, del tamaño justo, a un temporal en KLING_OUT_DIR
	// (builder_llm_capa.go). Con paquetes, el motor del constructor base.
	out := envOr("KLING_OUT_DIR", filepath.Join(root, "images"))
	if len(pkgs) == 0 {
		t0 := time.Now()
		r, err := construirCapaLLM(capaLLM{Name: req.Name, BaseImg: filepath.Join(root, "images", req.Base+".ext4"),
			Agent: agent, Rootfs: rootfs, Service: "/etc/von/run.sh", Out: out,
			T: time.Now().UTC().Truncate(time.Second), Reflink: cached, Modelo: "/models/" + res.File})
		if err != nil {
			return err
		}
		compartido := ""
		if r.Compartido > 0 {
			compartido = fmt.Sprintf(", %d MiB shared with the cache (reflink)", r.Compartido>>20)
		}
		fmt.Printf("imagen '%s' lista — capa sobre base '%s' (%d MiB, %d files, in %.1f s%s)\n",
			req.Name, req.Base, r.Stats.Bytes()>>20, r.Stats.Files, time.Since(t0).Seconds(), compartido)
		soltarCacheModelo(cached, st.Size(), keepCacheDe(req.Spec), res.URL == "", r.Compartido)
		return nil
	}
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("missing %s (installed by `make deploy`)", script)
	}
	grow := req.GrowMB
	if grow == 0 {
		grow = res.LayerMiB(st.Size())
	}
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		return err
	}
	cmd := exec.Command("bash", script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"NAME="+req.Name, "BASE="+req.Base, fmt.Sprintf("GROW=%d", grow),
		"PKGS="+strings.Join(pkgs, " "), "AGENT="+agent, "ENV_FILE="+envFile,
		"ROOTFS_DIR="+rootfs, "SERVICE=/etc/von/run.sh", "KLING_OUT_DIR="+out)
	if err := cmd.Run(); err != nil {
		return err
	}
	soltarCacheModelo(cached, st.Size(), keepCacheDe(req.Spec), res.URL == "", 0)
	return nil
}

// baseGlibcEnGo escribe la base glibc en dst.
var baseGlibcEnGo = func(root, lib, dst string) error { return fmt.Errorf("not implemented") }

// buildLLMBase construye la base glibc (Debian trixie) con 71-build-glibc-base.sh.
func buildLLMBase(root, lib, name string) error {
	script := filepath.Join(lib, "71-build-glibc-base.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("base image %q is missing and %s is not installed (make deploy installs it)", name, script)
	}
	if _, err := exec.LookPath("debootstrap"); err != nil {
		if _, err2 := os.Stat("/usr/sbin/debootstrap"); err2 != nil {
			return fmt.Errorf("base image %q is missing and building it needs debootstrap on the daemon host: apt-get install debootstrap", name)
		}
	}
	fmt.Printf("building base image %q (Debian trixie, first model only; takes a few minutes)...\n", name)
	cmd := exec.Command("bash", script, name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "KLING_ROOT="+root, "SUITE=trixie",
		"PKGS="+strings.Join(von.BasePackages, " "),
		// Sin puente MCP: una imagen VON no lo usa.
		"BRIDGE=/nonexistent")
	if err := cmd.Run(); err != nil {
		os.Remove(filepath.Join(root, "images", name+".ext4"))
		return fmt.Errorf("building base %q: %w", name, err)
	}
	return nil
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractLlama saca del tarball los ficheros de llamaKeep a dst, planos. El
// tarball trae un directorio llama-<tag>/ arriba; lo que no está justo debajo
// se ignora, y un enlace solo puede apuntar a un nombre del mismo directorio.
func extractLlama(tarball, dst string) (int, error) {
	f, err := os.Open(tarball)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		parts := strings.Split(strings.TrimPrefix(path.Clean(h.Name), "./"), "/")
		if len(parts) != 2 || !llamaKeep.MatchString(parts[1]) {
			continue
		}
		name := filepath.Join(dst, parts[1])
		switch h.Typeflag {
		case tar.TypeReg:
			out, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
			if err != nil {
				return n, err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, 256<<20)); err != nil {
				out.Close()
				return n, err
			}
			if err := out.Close(); err != nil {
				return n, err
			}
		case tar.TypeSymlink:
			if strings.Contains(h.Linkname, "/") || !llamaKeep.MatchString(h.Linkname) {
				return n, fmt.Errorf("unexpected link %s -> %s", h.Name, h.Linkname)
			}
			os.Remove(name)
			if err := os.Symlink(h.Linkname, name); err != nil {
				return n, err
			}
		default:
			continue
		}
		n++
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
