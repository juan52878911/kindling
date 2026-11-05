package guest

// dm-verity en el init en Go: lo que hace el bloque de verity del init de sh
// (internal/imagen verityBlock, con dmsetup), con los ioctl de device-mapper
// sobre /dev/mapper/control y sin pedir nada a la imagen.
//
// Falla CERRADO como el script: si la base trae la tabla de verity de la capa
// y no se puede poner el dispositivo, el init se para y la capa no se monta
// sin verificar. Un fichero de tabla que existe y no se puede leer también
// para (el script lo habría tomado por ausente con su [ -f ]).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

const (
	// VerityConfPath y VerityDM son la tabla de verity de la capa y el nombre
	// de su dispositivo de device-mapper: los de la Marca de los constructores
	// debian y oci (internal/imagen, Marca.VerityConf y Marca.DM). Una base
	// con otra Marca lleva el init de sh, que los lleva escritos dentro.
	VerityConfPath = "/etc/kindling/layer.verity"
	VerityDM       = "kindling-layer"
	// verityDevMark es lo que la tabla dice en vez del dispositivo de la capa.
	verityDevMark = "@DEV@"
)

// dmTarget es una línea de una tabla de device-mapper: "inicio longitud
// tipo parámetros", en sectores de 512 bytes.
type dmTarget struct {
	start, length uint64
	kind, params  string
}

// parseDMTable lee la tabla como el script (grep -v '^#' y sed @DEV@): las
// líneas que no empiezan por "#" y no están vacías, con verityDevMark
// cambiado por dev. Sin ninguna, error: una tabla vacía no verifica nada.
func parseDMTable(s, dev string) ([]dmTarget, error) {
	var out []dmTarget
	for i, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(strings.ReplaceAll(line, verityDevMark, dev))
		if len(f) < 4 {
			return nil, fmt.Errorf("line %d: not \"start length type params\"", i+1)
		}
		start, err1 := strconv.ParseUint(f[0], 10, 64)
		length, err2 := strconv.ParseUint(f[1], 10, 64)
		if err1 != nil || err2 != nil || length == 0 {
			return nil, fmt.Errorf("line %d: bad start or length", i+1)
		}
		if len(f[2]) >= dmMaxTypeName {
			return nil, fmt.Errorf("line %d: target type too long", i+1)
		}
		out = append(out, dmTarget{start: start, length: length, kind: f[2], params: strings.Join(f[3:], " ")})
	}
	if len(out) == 0 {
		return nil, errors.New("the table has no targets")
	}
	return out, nil
}

// initVerity pone la capa layer detrás de dm-verity si la base trae su
// tabla (VerityConfPath) y devuelve el dispositivo que hay que montar: el de
// device-mapper, o layer tal cual si no hay tabla. Cualquier fallo con tabla
// es un error, y el arranque se para.
func initVerity(s initSys, layer string) (string, error) {
	b, err := s.ReadFile(VerityConfPath)
	if errors.Is(err, fs.ErrNotExist) {
		return layer, nil
	}
	fallo := func(err error) error {
		return fmt.Errorf("dm-verity on %s failed; refusing to mount the layer unverified: %w", layer, err)
	}
	if err != nil {
		return "", fallo(err)
	}
	targets, err := parseDMTable(string(b), layer)
	if err != nil {
		return "", fallo(fmt.Errorf("%s: %w", VerityConfPath, err))
	}
	dev, err := s.DMCreate(VerityDM, targets)
	if err != nil {
		return "", fallo(err)
	}
	return dev, nil
}

// Lo de <linux/dm-ioctl.h> que hace falta. struct dm_ioctl mide 312 bytes
// en todas las arquitecturas (sin huecos que dependan de ella), y
// dm_target_spec 40.
const (
	dmIoctlSize      = 312
	dmTargetSpecSize = 40
	dmNameLen        = 128
	dmMaxTypeName    = 16
	dmVersionMajor   = 4

	dmReadonlyFlag = 1 << 0

	// _IOWR(0xfd, nr, struct dm_ioctl): dirección 3 (lee y escribe), tamaño
	// 312, tipo 0xfd. La misma codificación en amd64 y arm64.
	dmIoctlBase   = 3<<30 | dmIoctlSize<<16 | 0xfd<<8
	dmDevCreate   = dmIoctlBase | 3
	dmDevRemove   = dmIoctlBase | 4
	dmDevSuspend  = dmIoctlBase | 6
	dmTableLoad   = dmIoctlBase | 9
	dmNameOffset  = 48
	dmDevOffset   = 40
	dmFlagsOffset = 28
)

// dmHeader es un struct dm_ioctl para name, con hueco detrás para extra
// bytes de datos (que van desde data_start = 312).
func dmHeader(name string, flags uint32, targets, extra int) ([]byte, error) {
	if name == "" || len(name) >= dmNameLen || strings.ContainsRune(name, '/') {
		return nil, fmt.Errorf("bad device-mapper name %q", name)
	}
	b := make([]byte, dmIoctlSize+extra)
	le := binary.NativeEndian // los ioctl van en el orden de la máquina
	le.PutUint32(b[0:], dmVersionMajor)
	// version[1] y [2] a 0: el kernel acepta cualquier menor >= 0.
	le.PutUint32(b[12:], uint32(len(b))) // data_size
	le.PutUint32(b[16:], dmIoctlSize)    // data_start
	le.PutUint32(b[20:], uint32(targets))
	le.PutUint32(b[dmFlagsOffset:], flags)
	copy(b[dmNameOffset:dmNameOffset+dmNameLen], name)
	return b, nil
}

// dmTableLoadBuf es el DM_TABLE_LOAD de targets para name, de solo lectura
// (dm-verity no acepta otra cosa): tras la cabecera, cada dm_target_spec con
// sus parámetros terminados en NUL, alineado a 8; next es el desplazamiento
// de una spec a la siguiente.
func dmTableLoadBuf(name string, targets []dmTarget) ([]byte, error) {
	sizes := make([]int, len(targets))
	extra := 0
	for i, t := range targets {
		if strings.IndexByte(t.params, 0) >= 0 || strings.IndexByte(t.kind, 0) >= 0 {
			return nil, errors.New("NUL in the table")
		}
		n := dmTargetSpecSize + len(t.params) + 1
		n = (n + 7) &^ 7
		sizes[i] = n
		extra += n
	}
	b, err := dmHeader(name, dmReadonlyFlag, len(targets), extra)
	if err != nil {
		return nil, err
	}
	le := binary.NativeEndian
	off := dmIoctlSize
	for i, t := range targets {
		sp := b[off:]
		le.PutUint64(sp[0:], t.start)
		le.PutUint64(sp[8:], t.length)
		le.PutUint32(sp[20:], uint32(sizes[i])) // next
		copy(sp[24:24+dmMaxTypeName], t.kind)
		copy(sp[dmTargetSpecSize:], t.params)
		off += sizes[i]
	}
	return b, nil
}

// dmDecodeDev es el dev_t que devuelve DM_DEV_CREATE (huge_encode_dev) como
// major y minor.
func dmDecodeDev(dev uint64) (major, minor uint32) {
	return uint32((dev & 0xfff00) >> 8), uint32((dev & 0xff) | ((dev >> 12) & 0xfff00))
}
