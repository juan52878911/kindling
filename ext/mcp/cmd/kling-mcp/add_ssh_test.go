package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
)

// sshFalso pone en el PATH un `ssh` que apunta sus argumentos, uno por linea, y
// devuelve la ruta del fichero donde los deja.
func sshFalso(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("el ssh falso es un guion de sh")
	}
	dir := t.TempDir()
	salida := filepath.Join(dir, "argv")
	guion := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + salida + "'; done\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(guion), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return salida
}

func leerArgv(t *testing.T, f string) []string {
	t.Helper()
	b, err := os.ReadFile(f)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// Un destino que empieza por "-" ssh lo leeria como una opcion suya: hay que
// rechazarlo antes de lanzar nada, como ya hace internal/transport.
func TestRunImportSSHRechazaDestinoConGuion(t *testing.T) {
	salida := sshFalso(t)
	err := runImport("ssh://-oAlgo=x", "svc", nil, &config.Config{})
	if err == nil {
		t.Fatal("un destino que empieza por - debe dar error")
	}
	if argv := leerArgv(t, salida); argv != nil {
		t.Errorf("no debe llegar a lanzar ssh: %v", argv)
	}
}

// ssh junta los argumentos remotos con espacios y se los da a la shell del otro
// lado: cada uno tiene que ir entrecomillado o un punto de montaje o un egress
// de la configuracion con espacios o metacaracteres se parte o se interpreta.
// Y el destino va tras "--", con la ruta del kling remoto si la URL la trae.
func TestRunImportSSHEntrecomillaYRespetaLaRuta(t *testing.T) {
	salida := sshFalso(t)
	vols := []api.VolumeAttachment{{Name: "datos", Mount: "/mnt/con espacio;x"}}
	cfg := &config.Config{Defaults: config.Defaults{Egress: "a b"}}
	if err := runImport("ssh://juan@lab/opt/bin/kling", "svc", vols, cfg); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	argv := leerArgv(t, salida)

	i := slices.Index(argv, "--")
	if i < 0 || i+1 >= len(argv) || argv[i+1] != "juan@lab" {
		t.Fatalf("el destino debe ir tras --: %v", argv)
	}
	remoto := argv[i+2:]
	if len(remoto) == 0 || remoto[0] != "'/opt/bin/kling'" {
		t.Errorf("debe usarse el kling de la ruta de la URL: %v", remoto)
	}
	for _, quiero := range []string{"'datos:/mnt/con espacio;x'", "'a b'"} {
		if !slices.Contains(remoto, quiero) {
			t.Errorf("falta %s entrecomillado en %v", quiero, remoto)
		}
	}
	for _, a := range remoto {
		if !strings.HasPrefix(a, "'") || !strings.HasSuffix(a, "'") {
			t.Errorf("argumento remoto sin entrecomillar: %q en %v", a, remoto)
		}
	}
}
