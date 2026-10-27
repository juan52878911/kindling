package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/pkg/api"
)

// `kling save -audit <ref>`: dice qué ficheros de la máquina no se han abierto
// mientras ha estado cargada, para decidir qué recortar. No guarda ni quita
// nada.
//
// La raíz de la imagen se monta de solo lectura (root=/dev/vda ro), así que
// no hay atime que mirar. Lo que sí se ve desde dentro, con las herramientas
// de busybox, es lo que está en uso en el momento: lo que cada proceso tiene
// mapeado (/proc/PID/maps: ejecutables y bibliotecas, que es casi todo el
// peso) y abierto (/proc/PID/fd, exe, cwd). Se muestrea durante -audit-for.
// Por eso "no visto" quiere decir "ni mapeado ni abierto en ninguna muestra":
// un fichero que se lee y se cierra entre dos muestras (una configuración, un
// catálogo) puede salir como no usado. Es una lista de candidatos, no una
// prueba.

// auditScript muestrea /proc y luego lista los ficheros de la imagen. $1 son
// los segundos de muestreo. Los ficheros que no son de la imagen (más nuevos
// que /etc/kindling/oci.json: datos escritos en esta máquina) no salen.
const auditScript = `
S=${1:-5}
U=/tmp/.kling-audit-used
: > $U
i=0
while [ $i -lt $S ]; do
  cat /proc/[0-9]*/maps 2>/dev/null | while read -r a b c d e f; do
    case "$f" in /*) echo "$f" ;; esac
  done >> $U
  for l in /proc/[0-9]*/fd/* /proc/[0-9]*/exe /proc/[0-9]*/cwd; do
    readlink "$l" 2>/dev/null
  done >> $U
  i=$((i+1))
  [ $i -lt $S ] && sleep 1
done
sort -u $U | while read -r f; do echo "U $f"; done
rm -f $U
M=/etc/kindling/oci.json
if [ -e $M ]; then
  find / -xdev -type f ! -newer $M -exec stat -c 'F %s %n' {} +
else
  find / -xdev -type f -exec stat -c 'F %s %n' {} +
fi
`

type auditFile struct {
	Path string
	Size int64
}

// parseAudit separa la salida del guion en la lista de ficheros y el conjunto
// de rutas vistas.
func parseAudit(out []byte) (files []auditFile, used map[string]bool) {
	used = map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "U /"):
			used[line[2:]] = true
		case strings.HasPrefix(line, "F "):
			sz, p, ok := strings.Cut(line[2:], " ")
			n, err := strconv.ParseInt(sz, 10, 64)
			if ok && err == nil && strings.HasPrefix(p, "/") {
				files = append(files, auditFile{p, n})
			}
		}
	}
	return files, used
}

// informeAuditoria es la salida legible. rules puede ser nil.
func informeAuditoria(ref string, files []auditFile, used map[string]bool, rules *imagen.ReglasSlim) string {
	var total, unusedBytes int64
	var unused []auditFile
	for _, f := range files {
		total += f.Size
		if !used[f.Path] {
			unused = append(unused, f)
			unusedBytes += f.Size
		}
	}
	var b strings.Builder
	pct := 0
	if total > 0 {
		pct = int(100 * unusedBytes / total)
	}
	fmt.Fprintf(&b, "audit of %s: %d files, %s in the image\n", ref, len(files), human(total))
	fmt.Fprintf(&b, "  never seen mapped or open: %d files, %s (%d%%)\n", len(unused), human(unusedBytes), pct)

	// Por directorio (tres niveles), lo más pesado primero.
	byDir := map[string]*auditFile{}
	counts := map[string]int{}
	for _, f := range unused {
		d := dirDe(f.Path, 3)
		if byDir[d] == nil {
			byDir[d] = &auditFile{Path: d}
		}
		byDir[d].Size += f.Size
		counts[d]++
	}
	dirs := make([]*auditFile, 0, len(byDir))
	for _, d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].Size != dirs[j].Size {
			return dirs[i].Size > dirs[j].Size
		}
		return dirs[i].Path < dirs[j].Path
	})
	b.WriteString("\n  by directory (unused, top 12):\n")
	for i, d := range dirs {
		if i == 12 {
			break
		}
		fmt.Fprintf(&b, "    %10s  %6d files  %s\n", human(d.Size), counts[d.Path], d.Path)
	}
	sort.Slice(unused, func(i, j int) bool {
		if unused[i].Size != unused[j].Size {
			return unused[i].Size > unused[j].Size
		}
		return unused[i].Path < unused[j].Path
	})
	b.WriteString("\n  biggest unused files:\n")
	for i, f := range unused {
		if i == 10 {
			break
		}
		fmt.Fprintf(&b, "    %10s  %s\n", human(f.Size), f.Path)
	}

	if rules != nil {
		if covered, fams, ok := cubreSlim(files, unused, rules); ok {
			fmt.Fprintf(&b, "\n  kling image import -slim (%s) already removes %s of the unused\n",
				strings.Join(fams, ", "), human(covered))
		}
	}
	b.WriteString("\n  A file read and closed between samples shows as unused: these are candidates, not proof.\n")
	b.WriteString("  Nothing was removed or saved.\n")
	return b.String()
}

// cubreSlim dice cuántos bytes de los no usados quitaría -slim (las reglas se
// aplican, en seco, a un árbol con los ficheros de la máquina).
func cubreSlim(files, unused []auditFile, rules *imagen.ReglasSlim) (int64, []string, bool) {
	now := time.Unix(0, 0)
	tree := ext4.NewDir(0o755, 0, 0, now)
	for _, f := range files {
		n := &ext4.Node{Mode: ext4.ModeReg | 0o644, Size: f.Size}
		if err := tree.Put(strings.TrimPrefix(f.Path, "/"), n, now); err != nil {
			continue
		}
	}
	inf, err := rules.Aplica(tree, nil, true)
	if err != nil || len(inf.Familias) == 0 {
		return 0, nil, false
	}
	var covered int64
	for _, f := range unused {
		p := strings.TrimPrefix(f.Path, "/")
		for _, r := range inf.Rutas {
			if p == r || strings.HasPrefix(p, r+"/") {
				covered += f.Size
				break
			}
		}
	}
	return covered, inf.Familias, true
}

// dirDe corta p a sus n primeros componentes de directorio.
func dirDe(p string, n int) string {
	parts := strings.Split(strings.TrimPrefix(path.Dir(p), "/"), "/")
	if len(parts) > n {
		parts = parts[:n]
	}
	return "/" + strings.Join(parts, "/")
}

// cmdSaveAudit corre el guion en la máquina y escribe el informe.
func cmdSaveAudit(ctx context.Context, c *api.Client, ref string, dur time.Duration) error {
	secs := max(1, int(dur.Seconds()))
	var out bytes.Buffer
	res, err := c.Exec(ctx, ref, api.ExecRequest{
		Cmd:            []string{"sh", "-c", auditScript, "audit", strconv.Itoa(secs)},
		TimeoutSeconds: secs + 120,
		MaxOutputBytes: 64 << 20,
	}, func(stream string, data []byte) error {
		if stream == "stdout" {
			out.Write(data)
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "exec is not enabled") {
			err = fmt.Errorf("%w\n(-audit needs a machine created with kling run -allow-exec, or a sandbox)", err)
		}
		return fmt.Errorf("%s: %w", ref, err)
	}
	if res.ExitCode != 0 || res.Truncated {
		return fmt.Errorf("%s: the audit script failed (exit %d, truncated=%v): %s", ref, res.ExitCode, res.Truncated, strings.TrimSpace(string(res.Stderr)))
	}
	files, used := parseAudit(out.Bytes())
	if len(files) == 0 {
		return fmt.Errorf("%s: the guest listed no files", ref)
	}
	rules, err := imagen.CargarReglasSlim()
	var rp *imagen.ReglasSlim
	if err == nil {
		rp = &rules
	}
	fmt.Print(informeAuditoria(ref, files, used, rp))
	return nil
}
