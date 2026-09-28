package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Cell agrupa las R rondas de un (modo, N). Sus percentiles son sobre TODAS
// las copias correctas de todas las rondas juntas (no un promedio de
// percentiles); las rondas siguen todas en Report.Rounds.
type Cell struct {
	Mode    string         `json:"mode"`
	N       int            `json:"n"`
	Status  string         `json:"status"` // ok | DEGRADED | skipped
	Reason  string         `json:"reason,omitempty"`
	Rounds  int            `json:"rounds"`
	Copies  int            `json:"copies"`
	OK      int            `json:"ok"`
	Failed  int            `json:"failed"`
	Errors  map[string]int `json:"errors,omitempty"`
	Latency Dist           `json:"latency_ms"`
	// MemAvailMinMiB: el mínimo de MemAvailable de todas las rondas; MemPeakUsedMiB,
	// el mayor descenso respecto del inicio de su ronda.
	MemAvailMinMiB int64   `json:"mem_available_min_mib"`
	MemPeakUsedMiB int64   `json:"mem_peak_used_mib"`
	DiskPerCopyMiB float64 `json:"disk_per_copy_mib"` // mediana de las rondas medidas; -1 = n/d
}

// Report es el informe completo, en JSON y en Markdown.
type Report struct {
	Schema      int       `json:"schema"`
	Started     time.Time `json:"started"`
	Finished    time.Time `json:"finished"`
	Interrupted bool      `json:"interrupted,omitempty"`
	Host        HostInfo  `json:"host"`
	Config      Public    `json:"config"`
	Preflight   []string  `json:"preflight"`
	Rounds      []Round   `json:"rounds"`
	Cells       []Cell    `json:"cells"`
}

// Public es la parte de la configuración que viaja con el informe (sin
// contraseñas ni rutas de secretos).
type Public struct {
	Modes       []string `json:"modes"`
	Ns          []int    `json:"n"`
	Reps        int      `json:"reps"`
	TimeoutS    float64  `json:"timeout_s"`
	Golden      string   `json:"golden,omitempty"`
	ForkSrc     string   `json:"fork_src,omitempty"`
	DockerImage string   `json:"docker_image,omitempty"`
	Table       string   `json:"table"`
	Expect      int64    `json:"expect_count"`
	SeedSQL     string   `json:"seed_sql"` // "embedded" o la ruta
	DiskPaths   string   `json:"disk_path,omitempty"`
	PSIMax      float64  `json:"psi_max"`
	EstMemMiB   int      `json:"est_mem_mib,omitempty"`
	EstDiskMiB  int      `json:"est_disk_mib,omitempty"`
}

const schemaVersion = 1

// buildCells agrupa rondas por (modo, N) manteniendo el orden de aparición.
func buildCells(rounds []Round) []Cell {
	type key struct {
		m string
		n int
	}
	var order []key
	by := map[key][]Round{}
	for _, r := range rounds {
		k := key{r.Mode, r.N}
		if _, ok := by[k]; !ok {
			order = append(order, k)
		}
		by[k] = append(by[k], r)
	}
	var cells []Cell
	for _, k := range order {
		rs := by[k]
		c := Cell{Mode: k.m, N: k.n, MemAvailMinMiB: -1, MemPeakUsedMiB: -1, DiskPerCopyMiB: -1}
		if len(rs) == 1 && rs[0].Status == "skipped" {
			c.Status, c.Reason = "skipped", rs[0].Reason
			cells = append(cells, c)
			continue
		}
		var lat, disks []float64
		for _, r := range rs {
			c.Rounds++
			c.Copies += r.OK + r.Failed
			c.OK += r.OK
			c.Failed += r.Failed
			for _, cp := range r.Copies {
				if cp.OK {
					lat = append(lat, cp.MS)
				}
			}
			for e, n := range r.Errors {
				if c.Errors == nil {
					c.Errors = map[string]int{}
				}
				c.Errors[e] += n
			}
			if r.MemAvail.Min >= 0 && (c.MemAvailMinMiB < 0 || r.MemAvail.Min < c.MemAvailMinMiB) {
				c.MemAvailMinMiB = r.MemAvail.Min
			}
			if r.MemAvail.PeakUsed > c.MemPeakUsedMiB {
				c.MemPeakUsedMiB = r.MemAvail.PeakUsed
			}
			if r.DiskPerCopyMiB >= 0 {
				disks = append(disks, r.DiskPerCopyMiB)
			}
		}
		c.Latency = summarize(lat)
		c.Status = failStatus(c.Failed, c.Copies)
		if len(disks) > 0 {
			sort.Float64s(disks)
			c.DiskPerCopyMiB = percentile(disks, 50)
		}
		cells = append(cells, c)
	}
	return cells
}

func numOrNA(v float64, na float64) string {
	if v == na {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", v)
}

func miB(v int64) string {
	if v < 0 {
		return "n/a"
	}
	return fmt.Sprint(v)
}

// writeMarkdown escribe el informe con las reglas de honestidad a la vista.
func writeMarkdown(w io.Writer, rep *Report) {
	h := rep.Host
	fmt.Fprintf(w, "# kling-dbbench: a throwaway database per test\n\n")
	fmt.Fprintf(w, "Started %s, finished %s.", rep.Started.UTC().Format(time.RFC3339), rep.Finished.UTC().Format(time.RFC3339))
	if rep.Interrupted {
		fmt.Fprint(w, " **INTERRUPTED: the report is partial.**")
	}
	fmt.Fprintf(w, "\n\n**Host:** %s, kernel %s, %s, %d CPUs, %d MiB RAM, nested=%v, container=%s, kling=%q, docker=%q, bench=%s\n\n",
		h.Hostname, h.Kernel, h.CPU, h.CPUs, h.MemTotalMiB, h.Nested, h.Container, h.Kling, h.Docker, h.GitSHA)
	fmt.Fprintf(w, "**Measured:** time from asking for a ready database (schema and seed) to `SELECT count(*) FROM %s` returning %d, for N copies asked at once. Modes: %s. N: %v. R = %d.\n\n",
		rep.Config.Table, rep.Config.Expect, strings.Join(rep.Config.Modes, ", "), rep.Config.Ns, rep.Config.Reps)
	if len(rep.Preflight) > 0 {
		fmt.Fprintln(w, "**Preflight:**")
		for _, p := range rep.Preflight {
			fmt.Fprintf(w, "- %s\n", p)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## Cells (all rounds pooled)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| mode | N | status | ok/total | p50 ms | p95 ms | p99 ms | max ms | MemAvailable min MiB | peak RAM used MiB | disk/copy MiB | errors |")
	fmt.Fprintln(w, "|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|")
	for _, c := range rep.Cells {
		if c.Status == "skipped" {
			fmt.Fprintf(w, "| %s | %d | skipped | - | - | - | - | - | - | - | - | %s |\n", c.Mode, c.N, c.Reason)
			continue
		}
		fmt.Fprintf(w, "| %s | %d | %s | %d/%d | %.1f | %.1f | %.1f | %.1f | %s | %s | %s | %s |\n",
			c.Mode, c.N, c.Status, c.OK, c.Copies, c.Latency.P50, c.Latency.P95, c.Latency.P99, c.Latency.Max,
			miB(c.MemAvailMinMiB), miB(c.MemPeakUsedMiB), numOrNA(c.DiskPerCopyMiB, -1), errorsSummary(c.Errors))
	}

	fmt.Fprintln(w, "\n## Every round")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| mode | N | rep | status | ok/N | p50 ms | p95 ms | p99 ms | max ms | MemAvailable before→min MiB | disk/copy MiB | PSI before/max | errors |")
	fmt.Fprintln(w, "|---|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|---|")
	for _, r := range rep.Rounds {
		if r.Status == "skipped" {
			fmt.Fprintf(w, "| %s | %d | - | skipped | - | - | - | - | - | - | - | - | %s |\n", r.Mode, r.N, r.Reason)
			continue
		}
		fmt.Fprintf(w, "| %s | %d | %d | %s | %d/%d | %.1f | %.1f | %.1f | %.1f | %s→%s | %s | %s/%s | %s |\n",
			r.Mode, r.N, r.Rep, r.Status, r.OK, r.OK+r.Failed, r.Latency.P50, r.Latency.P95, r.Latency.P99, r.Latency.Max,
			miB(r.MemAvail.Before), miB(r.MemAvail.Min), numOrNA(r.DiskPerCopyMiB, -1),
			numOrNA(r.PSIBefore, -1), numOrNA(r.PSIMax, -1), errorsSummary(r.Errors))
	}

	fmt.Fprintln(w, `
## How to read this

- Every measured round is in the table, failed or not. Nothing was rerun or trimmed to look better.
- A round or cell with more than 1% failed copies is **DEGRADED**: its percentiles are over the copies that finished, so they are not the latency of that cell.
- A cell whose estimate does not fit is **skipped**, with the arithmetic in the errors column.
- Percentiles are nearest-rank over the full sorted sample. Cell rows pool every good copy of every round.
- Latency starts when the whole burst is released and stops when the first `+"`count(*)`"+` returns the expected value. It includes the tool, the wait for the database and, for docker and template, applying the schema and seed.
- Disk per copy is the change in used space of the disk-path filesystem (after sync) divided by the copies that succeeded; other writers on that filesystem add noise, and "n/a" means it was not measured.
- Nested virtualization and containers change these numbers: compare only runs whose host header matches.`)
}
