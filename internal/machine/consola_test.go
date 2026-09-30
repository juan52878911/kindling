package machine

// Tests de N3 (M-07/D-04/A-01): consola serie acotada. Ninguno necesita KVM,
// root ni el binario de firecracker: abrirConsola y rotarConsola operan solo
// sobre ficheros.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAbrirConsolaSobreviveATruncar prueba la razón de ser de O_APPEND: un
// truncado EN SITIO del mismo fichero (lo que hace rotarConsola, sin cerrar ni
// reabrir el descriptor) no debe dejar un hueco de ceros ni perder las
// escrituras posteriores del mismo escritor.
func TestAbrirConsolaSobreviveATruncar(t *testing.T) {
	dir := t.TempDir()
	f, err := abrirConsola(dir)
	if err != nil {
		t.Fatalf("abrirConsola: %v", err)
	}
	defer f.Close()

	if _, err := f.WriteString("antes de rotar\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("despues de rotar\n"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "firecracker.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "despues de rotar\n" {
		t.Fatalf("contenido tras truncar+escribir = %q; sin O_APPEND habría un hueco de ceros o se "+
			"pisaría en la posición vieja del descriptor", got)
	}
}

func TestRotarConsolaNoTocaUnLogPequeno(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "firecracker.log")
	if err := os.WriteFile(path, []byte("poco\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rotarConsola(dir); err != nil {
		t.Fatalf("rotarConsola: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "firecracker.log.1")); !os.IsNotExist(err) {
		t.Fatalf("un log por debajo del tope no debe producir firecracker.log.1 (err=%v)", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "poco\n" {
		t.Fatalf("el log pequeño no debía tocarse: %q, %v", got, err)
	}
}

func TestRotarConsolaSinLogNoFalla(t *testing.T) {
	if err := rotarConsola(t.TempDir()); err != nil {
		t.Fatalf("sin firecracker.log no debe ser un error (máquina recién creada): %v", err)
	}
}

// TestRotarConsolaConservaLaColaYTruncaEnSitio es el caso central: al pasar el
// tope se conserva la cola real en firecracker.log.1, el fichero principal
// queda a 0, y un escritor con el descriptor ya abierto (como Firecracker,
// que nunca lo reabre) sigue escribiendo correctamente después.
func TestRotarConsolaConservaLaColaYTruncaEnSitio(t *testing.T) {
	dir := t.TempDir()
	f, err := abrirConsola(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	relleno := bytes.Repeat([]byte("x"), int(consolaMaxBytes)+1024)
	cola := []byte("ESTA-ES-LA-COLA-REAL\n")
	if _, err := f.Write(relleno); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(cola); err != nil {
		t.Fatal(err)
	}

	if err := rotarConsola(dir); err != nil {
		t.Fatalf("rotarConsola: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, "firecracker.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("firecracker.log debía quedar en 0 tras rotar, tiene %d bytes", fi.Size())
	}

	rotado, err := os.ReadFile(filepath.Join(dir, "firecracker.log.1"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rotado)) != consolaKeepBytes {
		t.Fatalf("firecracker.log.1 = %d bytes, quería exactamente consolaKeepBytes (%d)", len(rotado), consolaKeepBytes)
	}
	if !bytes.HasSuffix(rotado, cola) {
		t.Fatal("firecracker.log.1 no conserva la cola real escrita justo antes de rotar")
	}

	if _, err := f.WriteString("tras rotar\n"); err != nil {
		t.Fatal(err)
	}
	final, err := os.ReadFile(filepath.Join(dir, "firecracker.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(final) != "tras rotar\n" {
		t.Fatalf("tras rotar y volver a escribir, firecracker.log = %q; quería solo lo nuevo, sin huecos", final)
	}
}

// TestRotarConsolasRotaSoloLoQuePasaElTope comprueba el nivel de Manager que
// llama el vigilante: recoge los directorios de las máquinas conocidas bajo
// m.mu y rota cada una fuera del candado.
func TestRotarConsolasRotaSoloLoQuePasaElTope(t *testing.T) {
	m := newTestManager(t)
	idGrande, idPequeno := "grande0000000000000", "pequeno0000000000000"

	for _, id := range []string{idGrande, idPequeno} {
		if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
			t.Fatal(err)
		}
		m.addForTest(id)
	}

	if err := os.WriteFile(filepath.Join(m.dir(idGrande), "firecracker.log"),
		bytes.Repeat([]byte("y"), int(consolaMaxBytes)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir(idPequeno), "firecracker.log"),
		[]byte("poco\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.rotarConsolas()

	if _, err := os.Stat(filepath.Join(m.dir(idGrande), "firecracker.log.1")); err != nil {
		t.Fatalf("la máquina que pasaba el tope debía rotar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.dir(idPequeno), "firecracker.log.1")); !os.IsNotExist(err) {
		t.Fatalf("la máquina pequeña no debía rotar (err=%v)", err)
	}
}

// El barrido de directorios huérfanos (sweepMachineDirs) opera por DIRECTORIO
// entero de machines/, nunca por fichero: firecracker.log.1 vive dentro del
// directorio de una máquina conocida, nunca como entrada propia de machines/,
// así que no puede confundirse con basura mientras esa máquina siga en byID.
func TestSweepMachineDirsNoTocaFirecrackerLog1DeUnaMaquinaConocida(t *testing.T) {
	m := newTestManager(t)
	id := "conocida0000000000000"
	dir, unreserve, err := m.makeMachineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	unreserve()
	m.addForTest(id)

	if err := os.WriteFile(filepath.Join(dir, "firecracker.log.1"), []byte("cola vieja"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.sweepMachineDirs()
	m.sweepSnapshotLeftovers()
	m.mu.Unlock()

	got, err := os.ReadFile(filepath.Join(dir, "firecracker.log.1"))
	if err != nil {
		t.Fatalf("el barrido se llevó firecracker.log.1 de una máquina conocida: %v", err)
	}
	if string(got) != "cola vieja" {
		t.Fatalf("firecracker.log.1 cambió de contenido: %q", got)
	}
}

// Con KLING_JAILER=0 el VMM ve el disco del host y el directorio de su
// máquina es suyo: puede cambiar firecracker.log (o .log.1) por un enlace a
// un fichero del host. El daemon, que es root, no puede truncarlo, ni
// escribir la consola del invitado en él, ni devolverlo por Logs.
func TestConsolaNoSigueEnlacesPlantados(t *testing.T) {
	fichHost := func(t *testing.T, tam int64) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "del-host")
		if err := os.WriteFile(p, []byte("contenido del host\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if tam > 0 {
			if err := os.Truncate(p, tam); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	tamDe := func(t *testing.T, p string) int64 {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}

	t.Run("abrirConsola no trunca el destino", func(t *testing.T) {
		dir, host := t.TempDir(), fichHost(t, 0)
		if err := os.Symlink(host, filepath.Join(dir, "firecracker.log")); err != nil {
			t.Fatal(err)
		}
		f, err := abrirConsola(dir)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if b, _ := os.ReadFile(host); string(b) != "contenido del host\n" {
			t.Fatalf("abrirConsola truncó el fichero del host: %q", b)
		}
		if fi, err := os.Lstat(filepath.Join(dir, "firecracker.log")); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("firecracker.log no es un fichero nuevo: %v", err)
		}
	})

	t.Run("rotar no trunca ni copia el destino", func(t *testing.T) {
		dir, host := t.TempDir(), fichHost(t, consolaMaxBytes+4096)
		if err := os.Symlink(host, filepath.Join(dir, "firecracker.log")); err != nil {
			t.Fatal(err)
		}
		if err := rotarConsola(dir); err == nil {
			t.Error("rotarConsola rotó un enlace sin quejarse")
		}
		if got := tamDe(t, host); got != consolaMaxBytes+4096 {
			t.Fatalf("rotarConsola truncó el fichero del host: %d bytes", got)
		}
		if _, err := os.Lstat(filepath.Join(dir, "firecracker.log.1")); !os.IsNotExist(err) {
			t.Fatalf("copió el fichero del host a firecracker.log.1: %v", err)
		}
	})

	t.Run("rotar no trunca un hardlink", func(t *testing.T) {
		dir, host := t.TempDir(), fichHost(t, consolaMaxBytes+4096)
		if err := os.Link(host, filepath.Join(dir, "firecracker.log")); err != nil {
			t.Skip(err)
		}
		_ = rotarConsola(dir)
		if got := tamDe(t, host); got != consolaMaxBytes+4096 {
			t.Fatalf("rotarConsola truncó el fichero del host por un hardlink: %d bytes", got)
		}
	})

	t.Run("rotar no escribe a traves de firecracker.log.1", func(t *testing.T) {
		dir, host := t.TempDir(), fichHost(t, 0)
		f, err := abrirConsola(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(consolaMaxBytes + 4096); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(host, filepath.Join(dir, "firecracker.log.1")); err != nil {
			t.Fatal(err)
		}
		if err := rotarConsola(dir); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(host); string(b) != "contenido del host\n" {
			t.Fatalf("la consola del invitado se escribió en el fichero del host (%d bytes)", len(b))
		}
		if fi, err := os.Lstat(filepath.Join(dir, "firecracker.log.1")); err != nil || !fi.Mode().IsRegular() || fi.Size() != consolaKeepBytes {
			t.Fatalf("firecracker.log.1 no quedó como la cola rotada: %v", err)
		}
	})

	t.Run("Logs no lee el destino", func(t *testing.T) {
		dir, host := t.TempDir(), fichHost(t, 0)
		if err := os.Symlink(host, filepath.Join(dir, "firecracker.log")); err != nil {
			t.Fatal(err)
		}
		if b, err := leerCola(filepath.Join(dir, "firecracker.log"), logMaxBytes); err == nil {
			t.Fatalf("leerCola devolvió el fichero del host: %q", b)
		}
	})
}
