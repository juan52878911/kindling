package plugin_test

// La firma de SHA256SUMS en `kling plugin install`, con el verificador de
// internal/upgrade y una clave de prueba (paquete aparte: upgrade importa
// plugin).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/upgrade"
	"github.com/juan52878911/kindling/pkg/plugin"
)

var firmaAsset = "kling-demo-" + runtime.GOOS + "-" + runtime.GOARCH

const firmaManifest = `{"manifest_version":1,"name":"demo","version":"2.0.0","commands":[{"name":"demo"}]}`

func firmaBin() []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = --kling-manifest ]; then echo '" + firmaManifest + "'; exit 0; fi\n")
}

func firmaSums(bin []byte) []byte {
	h := sha256.Sum256(bin)
	return []byte(hex.EncodeToString(h[:]) + "  " + firmaAsset + "\n")
}

func clavePrueba(t *testing.T) upgrade.ClavePrivada {
	t.Helper()
	k, err := upgrade.GenerarClave(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func firmar(t *testing.T, k upgrade.ClavePrivada, sums []byte, tag string) []byte {
	t.Helper()
	f, err := upgrade.Firmar(k, sums, fmt.Sprintf("timestamp:1\tfile:SHA256SUMS\ttag:%s\thashed", tag))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// servirRelease sirve en /<tag>/ el binario, SHA256SUMS y, si sig no es nil,
// SHA256SUMS.minisig.
func servirRelease(t *testing.T, sums, sig []byte) *httptest.Server {
	t.Helper()
	bin := firmaBin()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case firmaAsset:
			w.Write(bin)
		case "SHA256SUMS":
			w.Write(sums)
		case plugin.SigName:
			if sig == nil {
				http.NotFound(w, r)
				return
			}
			w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func instalarFirmada(t *testing.T, srv *httptest.Server, o plugin.InstallOptions) (string, error) {
	t.Helper()
	plugin.ManifestTimeout = 10 * time.Second
	var avisos bytes.Buffer
	o.Name, o.Dir, o.Client, o.ReleaseURL, o.Warnings = "demo", t.TempDir(), srv.Client(), srv.URL, &avisos
	if o.CoreVersion == "" {
		o.CoreVersion = "1.0.0"
	}
	_, err := plugin.Install(context.Background(), o)
	return avisos.String(), err
}

func TestInstallFirma(t *testing.T) {
	k := clavePrueba(t)
	verif := upgrade.VerificadorDeSumas([]upgrade.ClavePublica{k.Publica()})
	sums := firmaSums(firmaBin())

	t.Run("firmada", func(t *testing.T) {
		srv := servirRelease(t, sums, firmar(t, k, sums, "v1.0.0"))
		if avisos, err := instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif}); err != nil || avisos != "" {
			t.Fatalf("a signed release: err %v, warnings %q", err, avisos)
		}
	})
	t.Run("sin firma desde la 1.0.0", func(t *testing.T) {
		srv := servirRelease(t, sums, nil)
		_, err := instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif})
		if err == nil || !strings.Contains(err.Error(), "signed from") {
			t.Fatalf("an unsigned v1.0.0 must be refused: %v", err)
		}
		// Aunque la release que se pide sea anterior: cuenta este kling.
		_, err = instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif, Tag: "v0.20.1"})
		if err == nil {
			t.Fatal("a 1.x kling must refuse an unsigned v0.20.1 too")
		}
	})
	t.Run("sin firma con -insecure", func(t *testing.T) {
		srv := servirRelease(t, sums, nil)
		avisos, err := instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif, Insecure: true})
		if err != nil || !strings.Contains(avisos, "-insecure") {
			t.Fatalf("-insecure: err %v, warnings %q", err, avisos)
		}
	})
	t.Run("sin firma antes de la 1.0.0", func(t *testing.T) {
		srv := servirRelease(t, sums, nil)
		avisos, err := instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif, Tag: "v0.20.1", CoreVersion: "0.20.1"})
		if err != nil || !strings.Contains(avisos, "warning") {
			t.Fatalf("before 1.0.0 only a warning: err %v, warnings %q", err, avisos)
		}
	})
	t.Run("otra clave", func(t *testing.T) {
		srv := servirRelease(t, sums, firmar(t, clavePrueba(t), sums, "v1.0.0"))
		// -insecure no salta una firma que está.
		if _, err := instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif, Insecure: true}); err == nil {
			t.Fatal("a signature from an untrusted key must be refused, -insecure or not")
		}
	})
	t.Run("otra release", func(t *testing.T) {
		srv := servirRelease(t, sums, firmar(t, k, sums, "v0.9.0"))
		if _, err := instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif}); err == nil || !strings.Contains(err.Error(), "v0.9.0") {
			t.Fatalf("a SHA256SUMS signed for another release must be refused: %v", err)
		}
	})
	t.Run("SHA256SUMS cambiado", func(t *testing.T) {
		sig := firmar(t, k, sums, "v1.0.0")
		otro := append([]byte(strings.Repeat("0", 64)+"  kling-otro\n"), sums...)
		srv := servirRelease(t, otro, sig)
		if _, err := instalarFirmada(t, srv, plugin.InstallOptions{VerifySums: verif}); err == nil {
			t.Fatal("a SHA256SUMS that does not match its signature must be refused")
		}
	})
	t.Run("sin verificador", func(t *testing.T) {
		srv := servirRelease(t, sums, firmar(t, k, sums, "v1.0.0"))
		if _, err := instalarFirmada(t, srv, plugin.InstallOptions{}); err == nil {
			t.Fatal("without a SumsVerifier a download must be refused")
		}
	})
}

// -file sigue funcionando sin firma (lo que se instala ya está en la
// máquina); una firma que está al lado se comprueba.
func TestInstallFirmaLocal(t *testing.T) {
	k := clavePrueba(t)
	verif := upgrade.VerificadorDeSumas([]upgrade.ClavePublica{k.Publica()})
	src := t.TempDir()
	bin := filepath.Join(src, firmaAsset)
	sums := firmaSums(firmaBin())
	os.WriteFile(bin, firmaBin(), 0o755)
	os.WriteFile(filepath.Join(src, "SHA256SUMS"), sums, 0o644)
	plugin.ManifestTimeout = 10 * time.Second
	o := plugin.InstallOptions{Name: "demo", File: bin, CoreVersion: "1.0.0", VerifySums: verif}

	o.Dir = t.TempDir()
	if _, err := plugin.Install(context.Background(), o); err != nil {
		t.Fatalf("-file without a signature: %v", err)
	}
	o.Dir, o.VerifySums = t.TempDir(), nil
	if _, err := plugin.Install(context.Background(), o); err != nil {
		t.Fatalf("-file without a signature or a verifier: %v", err)
	}
	o.VerifySums = verif
	os.WriteFile(filepath.Join(src, plugin.SigName), firmar(t, clavePrueba(t), sums, "v1.0.0"), 0o644)
	o.Dir = t.TempDir()
	if _, err := plugin.Install(context.Background(), o); err == nil {
		t.Fatal("-file with a signature from an untrusted key must be refused")
	}
	os.WriteFile(filepath.Join(src, plugin.SigName), firmar(t, k, sums, "v1.0.0"), 0o644)
	o.Dir = t.TempDir()
	if _, err := plugin.Install(context.Background(), o); err != nil {
		t.Fatalf("-file with a good signature: %v", err)
	}
}
