// firmar genera la clave de firma de releases y firma el SHA256SUMS de una
// release en el formato de minisign (docs/firmas.md). No es parte de kling:
// lo usan scripts/firma-clave.sh (en local, una vez por clave) y el paso
// «firmar SHA256SUMS» de .github/workflows/release.yml.
//
//	go run ./internal/upgrade/firmar genkey -dir DIR
//	KLING_RELEASE_SIGNING_KEY=… go run ./internal/upgrade/firmar sign -tag vX.Y.Z -in release/SHA256SUMS
//	go run ./internal/upgrade/firmar verify -tag vX.Y.Z -in SHA256SUMS [-sig SHA256SUMS.minisig] [-pub k.pub]
package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/juan52878911/kindling/internal/upgrade"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: firmar genkey|sign|verify [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "genkey":
		err = genkey(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "firmar:", err)
		os.Exit(1)
	}
}

func genkey(args []string) error {
	fs := flag.NewFlagSet("genkey", flag.ExitOnError)
	dir := fs.String("dir", ".", "where to write kling-release.key (secret) and kling-release.pub")
	_ = fs.Parse(args)
	k, err := upgrade.GenerarClave(rand.Reader)
	if err != nil {
		return err
	}
	sec := filepath.Join(*dir, "kling-release.key")
	pub := filepath.Join(*dir, "kling-release.pub")
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	// O_EXCL: no machacar una clave que ya existe por repetir el comando.
	fd, err := os.OpenFile(sec, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fd.Write(k.Fichero()); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(pub, k.Publica().Fichero(), 0o644); err != nil {
		return err
	}
	fmt.Printf("secret key: %s\npublic key: %s\nkey id:     %s\npublic key line (internal/upgrade/firma.go clavesRelease, scripts/install.sh KLING_RELEASE_PUBKEYS):\n%s\n",
		sec, pub, k.Publica().IDHex(), k.Publica().String())
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	tag := fs.String("tag", "", "release tag (goes in the trusted comment)")
	in := fs.String("in", "SHA256SUMS", "file to sign; the signature goes to <in>.minisig")
	_ = fs.Parse(args)
	if !upgrade.EtiquetaValida(*tag) {
		return fmt.Errorf("invalid -tag %q", *tag)
	}
	sec := os.Getenv("KLING_RELEASE_SIGNING_KEY")
	if sec == "" {
		return errors.New("KLING_RELEASE_SIGNING_KEY is empty")
	}
	k, err := upgrade.ParsearClavePrivada([]byte(sec))
	if err != nil {
		return err
	}
	datos, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	comentario := fmt.Sprintf("timestamp:%d\tfile:%s\ttag:%s\thashed", time.Now().Unix(), filepath.Base(*in), *tag)
	firma, err := upgrade.Firmar(k, datos, comentario)
	if err != nil {
		return err
	}
	// Una firma con una clave que los binarios de esta release no traen la
	// rechazarían todos los clientes que sí tienen claves: mejor no publicarla.
	if _, err := upgrade.VerificarFirma(datos, firma, upgrade.ClavesRelease()); err != nil {
		return fmt.Errorf("key %s is not in clavesRelease of this checkout: %w", k.Publica().IDHex(), err)
	}
	if err := os.WriteFile(*in+".minisig", firma, 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s with key %s\n", *in, k.Publica().IDHex())
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	tag := fs.String("tag", "", "expected release tag (empty = do not check)")
	in := fs.String("in", "SHA256SUMS", "signed file")
	sig := fs.String("sig", "", "signature (default <in>.minisig)")
	pub := fs.String("pub", "", "public key file (default: the keys built into kling)")
	_ = fs.Parse(args)
	if *sig == "" {
		*sig = *in + ".minisig"
	}
	claves := upgrade.ClavesRelease()
	if *pub != "" {
		b, err := os.ReadFile(*pub)
		if err != nil {
			return err
		}
		c, err := upgrade.ParsearClavePublica(string(b))
		if err != nil {
			return err
		}
		claves = []upgrade.ClavePublica{c}
	}
	datos, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	firma, err := os.ReadFile(*sig)
	if err != nil {
		return err
	}
	comentario, err := upgrade.VerificarFirma(datos, firma, claves)
	if err != nil {
		return err
	}
	if *tag != "" && upgrade.CampoComentario(comentario, "tag") != *tag {
		return fmt.Errorf("signed for %q, not %s", upgrade.CampoComentario(comentario, "tag"), *tag)
	}
	fmt.Printf("ok: %s\n", comentario)
	return nil
}
