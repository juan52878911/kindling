package main

// kling graph audit <g>: la auditoría de las aristas de un grafo en una sola
// línea de tiempo. Cada nodo tiene la suya (el registro del proxy de
// credenciales de su máquina, GET /machines/{ref}/credaudit): ahí quedan sus
// conexiones salientes por aristas link (kind link) y credential (kind
// postgres o mysql, con Host <nodo>.graph). Aquí se piden las de todos los
// nodos con máquina, se quedan las de las aristas (o todas con -all), se
// ordenan por tiempo y cada fila dice de qué nodo salió.
//
// Sin ruta nueva en el daemon: son las mismas lecturas que `kling machine
// audit`, una por nodo, así que la autorización es la de siempre (un inquilino
// solo lee las máquinas que son suyas) y el registro no lleva secretos (ni
// claves ni marcadores; ver api.CredAuditRecord).

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// registroGrafo es una línea de la auditoría de un grafo: el registro tal
// cual, con el nodo de origen delante (en JSON, "node" junto a los campos de
// siempre).
type registroGrafo struct {
	Node string `json:"node"`
	api.CredAuditRecord
}

// leerAuditoriaMaquina es CredAudit del cliente (sustituible en los tests).
type leerAuditoriaMaquina func(ctx context.Context, ref string, q api.CredAuditQuery) ([]api.CredAuditRecord, error)

// deArista dice si r es tráfico de una arista del grafo: una conexión link, o
// una de Postgres/MySQL hacia <nodo>.graph (una credential). Las de Postgres
// sin Host (rechazadas antes de saber qué credencial era) también, y los
// descartados siempre: una pérdida no se esconde tras un filtro.
func deArista(r api.CredAuditRecord) bool {
	switch r.Kind {
	case "link", "dropped":
		return true
	case "postgres", "mysql":
		return r.Host == "" || strings.HasSuffix(r.Host, "."+api.GraphDomain)
	}
	return false
}

// auditoriaGrafo junta la auditoría de los nodos de g con máquina en una
// línea de tiempo (las más antiguas primero; a igual instante, por nodo). Un
// nodo cuyo registro no se puede leer se avisa en avisos y no para a los
// demás; si no se puede leer ninguno, es un error. tail > 0 deja las últimas
// tail líneas del total.
func auditoriaGrafo(ctx context.Context, leer leerAuditoriaMaquina, g *api.Graph, q api.CredAuditQuery, todo bool, tail int, avisos io.Writer) ([]registroGrafo, error) {
	nodos := g.SortedNodeNames()
	// Con -all, las últimas tail del total están entre las últimas tail de
	// cada nodo; filtrando por arista, no: se leen enteros (el daemon ya
	// acota cada uno a 4 MiB).
	q.Tail = 0
	if todo {
		q.Tail = tail
	}
	var out []registroGrafo
	leidos, conMaquina := 0, 0
	var primero error
	for _, n := range nodos {
		id := g.Nodes[n].MachineID
		if id == "" {
			continue // un lazy sin instancia no ha abierto nada
		}
		conMaquina++
		recs, err := leer(ctx, id, q)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			fmt.Fprintf(avisos, "warning: node %s: %v\n", n, err)
			if primero == nil {
				primero = err
			}
			continue
		}
		leidos++
		for _, r := range recs {
			if todo || deArista(r) {
				out = append(out, registroGrafo{Node: n, CredAuditRecord: r})
			}
		}
	}
	if conMaquina > 0 && leidos == 0 {
		return nil, fmt.Errorf("no node's audit log could be read: %w", primero)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].TS.Equal(out[j].TS) {
			return out[i].TS.Before(out[j].TS)
		}
		return out[i].Node < out[j].Node
	})
	if tail > 0 && len(out) > tail {
		out = out[len(out)-tail:]
	}
	return out, nil
}

const graphAuditRowFmt = "%-14s  %-12s  %-7s  %-24s  %-32s  %6s  %-12s  %6s  %s\n"

// escribirAuditoriaGrafo pinta la línea de tiempo: una fila por registro (o
// una línea JSON con -json). Los descartados van a errOut en los dos modos.
func escribirAuditoriaGrafo(out, errOut io.Writer, recs []registroGrafo, asJSON bool) error {
	if !asJSON {
		fmt.Fprintf(out, graphAuditRowFmt, "TIME", "NODE", "METHOD", "HOST", "PATH", "STATUS", "CREDS", "MS", "RESULT")
	}
	enc := json.NewEncoder(out)
	for _, r := range recs {
		if r.Dropped > 0 {
			fmt.Fprintf(errOut, "node %s: dropped %d records\n", printable(r.Node), r.Dropped)
		}
		if asJSON {
			if err := enc.Encode(r); err != nil {
				return err
			}
			continue
		}
		if r.Kind == "dropped" {
			continue
		}
		method, path, status, creds := auditColumnas(r.CredAuditRecord)
		fmt.Fprintf(out, graphAuditRowFmt, r.TS.Local().Format("01-02 15:04:05"), printable(r.Node), printable(method),
			printable(r.Host), printable(path), status, printable(creds), fmt.Sprint(r.MS), printable(auditResult(r.CredAuditRecord)))
	}
	return nil
}

func graphAudit(args []string) error {
	fs := flag.NewFlagSet("graph audit", flag.ExitOnError)
	host := hostFlag(fs)
	tail := fs.Int("tail", 200, "last N records of the whole graph (0 = all the daemon keeps)")
	denied := fs.Bool("denied", false, "only connections denied by policy")
	since := fs.String("since", "", "only records since an RFC 3339 time or a duration back from now (10m, 2h)")
	all := fs.Bool("all", false, "also the nodes' other credential proxy traffic, not only their edges")
	asJSON := fs.Bool("json", false, "one JSON record per line, with its node")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *tail < 0 {
		return errors.New("usage: kling graph audit <graph> [-since 10m|RFC3339] [-denied] [-all] [-tail N] [-json]")
	}
	q := api.CredAuditQuery{Denied: *denied}
	if *since != "" {
		ts, err := parseSince(*since, time.Now())
		if err != nil {
			return err
		}
		q.Since = ts
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(hostOf(*host))
	g, err := c.Graph(ctx, fs.Arg(0))
	if err != nil {
		return errSinGrafos(err)
	}
	recs, err := auditoriaGrafo(ctx, c.CredAudit, g, q, *all, *tail, os.Stderr)
	if err != nil {
		return err
	}
	return escribirAuditoriaGrafo(os.Stdout, os.Stderr, recs, *asJSON)
}
