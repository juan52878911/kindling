// domotica-data descarga los conjuntos de datos libres para la tarea de
// domótica de kindling y los convierte a un esquema único con repartos
// train/valid/test sin fugas. Los datos no van al repositorio: se guardan en
// una caché; aquí solo viven la herramienta, la taxonomía (pkg/domotica) y la
// atribución (docs/domotica-datos.md, NOTICE).
//
//	go run ./tools/domotica-data fetch [-cache DIR]
//	go run ./tools/domotica-data build [-cache DIR] [-out DIR]
//
// Fuentes (versión fijada y sha256 comprobado ANTES de usar nada):
//   - Amazon MASSIVE 1.0 (CC BY 4.0): es-ES y en-US, repartos oficiales.
//   - home-assistant/intents (hoy OHF-Voice/intents, CC BY 4.0), etiqueta
//     2026.9.17: plantillas es/en expandidas de forma acotada y determinista.
//   - Las órdenes de la demo (pkg/domotica/demo.go), del propio proyecto.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// source es un archivo fijado por URL, tamaño y sha256.
type source struct {
	Name      string
	URL       string
	File      string
	SHA256    string
	MaxBytes  int64 // tope de la descarga
	MaxUnpack int64 // tope de lo descomprimido que se recorre
	License   string
	Version   string
}

var sources = []source{
	{
		Name:      "massive",
		URL:       "https://amazon-massive-nlu-dataset.s3.amazonaws.com/amazon-massive-dataset-1.0.tar.gz",
		File:      "amazon-massive-dataset-1.0.tar.gz",
		SHA256:    "7df623fd2d300a4d235d6ee5bd396c9a28258d3a0ccb29abdb054506eba153f8",
		MaxBytes:  64 << 20,
		MaxUnpack: 1 << 30,
		License:   "CC-BY-4.0",
		Version:   "1.0",
	},
	{
		Name:      "ha-intents",
		URL:       "https://codeload.github.com/OHF-Voice/intents/tar.gz/4af16c0ccc6f0567654c04554833fe3e5e7467ba",
		File:      "ohf-voice-intents-4af16c0c.tar.gz",
		SHA256:    "3da1f44c65ea06232712adb714af91d55b794a939d4506bc7977f700a1b9e2be",
		MaxBytes:  32 << 20,
		MaxUnpack: 256 << 20,
		License:   "CC-BY-4.0",
		Version:   "2026.9.17 (4af16c0ccc6f0567654c04554833fe3e5e7467ba)",
	},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = cmdFetch(os.Args[2:])
	case "build":
		err = cmdBuild(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  domotica-data fetch [-cache DIR]             download MASSIVE 1.0 and home-assistant/intents
                                               (pinned, sha256-verified) into the cache
  domotica-data build [-cache DIR] [-out DIR]  unified JSONL with train/valid/test splits
                                               (default -out: CACHE/data)
`)
	os.Exit(2)
}

func defaultCache() string {
	if d := os.Getenv("KLING_DOMOTICA_CACHE"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "kindling", "domotica")
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	cache := fs.String("cache", defaultCache(), "cache directory")
	_ = fs.Parse(args)
	if err := os.MkdirAll(*cache, 0o755); err != nil {
		return err
	}
	for _, s := range sources {
		path := filepath.Join(*cache, s.File)
		if ok, _ := verify(path, s); ok {
			fmt.Printf("%-10s ok (cached) %s\n", s.Name, path)
			continue
		}
		fmt.Printf("%-10s downloading %s\n", s.Name, s.URL)
		if err := download(s, path); err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		fmt.Printf("%-10s ok %s\n", s.Name, path)
	}
	return nil
}

func verify(path string, s source) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, s.MaxBytes+1)); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == s.SHA256, nil
}

// download baja a un temporal con tope de tamaño, comprueba el sha256 y solo
// entonces lo mueve a su sitio: un fichero a medias o alterado nunca queda en
// la caché con el nombre bueno.
func download(s source, path string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(s.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, s.MaxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if n > s.MaxBytes {
		os.Remove(tmp)
		return fmt.Errorf("download larger than %d bytes", s.MaxBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != s.SHA256 {
		os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, s.SHA256)
	}
	return os.Rename(tmp, path)
}

// readTar lee del .tar.gz las entradas que want acepta, con topes por entrada
// y por total descomprimido (una bomba gzip no se come la memoria). Se
// comprueba el sha256 del archivo antes de abrirlo.
func readTar(path string, s source, want func(name string) bool) (map[string][]byte, error) {
	if ok, err := verify(path, s); !ok {
		if err == nil {
			err = errors.New("sha256 mismatch (run fetch again)")
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(io.LimitReader(f, s.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	lim := &io.LimitedReader{R: gz, N: s.MaxUnpack}
	tr := tar.NewReader(lim)
	out := map[string][]byte{}
	const maxEntry = 64 << 20
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if lim.N <= 0 {
				return nil, fmt.Errorf("archive expands beyond %d bytes", s.MaxUnpack)
			}
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || !want(hdr.Name) {
			continue
		}
		if hdr.Size > maxEntry {
			return nil, fmt.Errorf("%s: entry larger than %d bytes", hdr.Name, maxEntry)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxEntry))
		if err != nil {
			return nil, err
		}
		out[hdr.Name] = b
	}
	return out, nil
}

// stripTop quita el primer componente de la ruta («intents-<commit>/…»).
func stripTop(name string) string {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}
