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
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/von"
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
var llamaKeep = regexp.MustCompile(`^(llama-server|LICENSE|libllama-server-impl\.so|libllama-common\.so.*|libmtmd\.so.*|libllama\.so.*|libggml\.so.*|libggml-base\.so.*|libggml-cpu-[A-Za-z0-9._-]+\.so)$`)

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
	for _, p := range []string{agent, script} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing %s (installed by `make deploy`)", p)
		}
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
	if err := fetchVerified(res.URL, res.SHA256, cached); err != nil {
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
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(etc, "model.json"), append(manifest, '\n'), 0o644); err != nil {
		return err
	}

	// 5. La capa, con el motor del constructor base.
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
		"ROOTFS_DIR="+rootfs, "SERVICE=/etc/von/run.sh")
	return cmd.Run()
}

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

// fetchVerified deja en dst el fichero de url con ese sha256. Si dst ya está y
// cuadra, no descarga nada. Escribe al lado y renombra: un corte a medias no
// deja en la caché un fichero que parezca bueno.
func fetchVerified(url, want, dst string) error {
	if got, err := sha256File(dst); err == nil {
		if got == want {
			return nil
		}
		os.Remove(dst)
	}
	tmp := dst + ".part"
	defer os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	t0 := time.Now()
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	h := sha256.New()
	nb, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", url, got, want)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	secs := time.Since(t0).Seconds()
	fmt.Printf("downloaded %s: %d MiB in %.1f s, sha256 verified\n", path.Base(dst), nb>>20, secs)
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
