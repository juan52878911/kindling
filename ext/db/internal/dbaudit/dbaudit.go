// Package dbaudit une en una línea de tiempo los eventos del daemon de una
// copia y las conexiones a su Postgres. Solo lee metadatos: instante, evento,
// usuario, base y cliente. Nunca SQL ni contraseñas (el golden no activa
// log_statement y aquí solo se analizan los mensajes de conexión conocidos).
package dbaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/klingc"
)

// Tope de líneas del log que se leen del invitado y ventana de espera de los
// eventos del daemon (el CLI solo ofrece un flujo en vivo, sin histórico).
const (
	logTailLines   = 5000
	pgLogPath      = "/var/log/postgresql/pg.log"
	eventsWindow   = 2 * time.Second
	maxOutputLines = 10000
)

// now se sustituye en los tests.
var now = time.Now

// Entry es una fila de la línea de tiempo.
type Entry struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	User   string    `json:"user,omitempty"`
	DB     string    `json:"db,omitempty"`
	Client string    `json:"client,omitempty"`
}

var machineRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Run escribe en w la línea de tiempo de machine de los últimos since.
func Run(ctx context.Context, k klingc.Kling, machine string, since time.Duration, jsonOut bool, w io.Writer) error {
	if !machineRe.MatchString(machine) {
		return fmt.Errorf("invalid machine name %q", machine)
	}
	if since <= 0 {
		return fmt.Errorf("since must be positive")
	}
	cutoff := now().Add(-since)

	out, err := k.Run(ctx, nil, "exec", "-timeout", "30s", machine, "--",
		"tail", "-n", fmt.Sprint(logTailLines), pgLogPath)
	if err != nil {
		return fmt.Errorf("reading the postgres log of %s: %w", machine, err)
	}
	entries := ParseLog(string(out))

	// Los eventos del daemon son un flujo en vivo: se escucha una ventana corta.
	// Es un extra; si falla, la línea de tiempo sigue con las conexiones.
	ectx, cancel := context.WithTimeout(ctx, eventsWindow)
	evOut, _ := k.Run(ectx, nil, "events", "-json")
	cancel()
	entries = append(entries, ParseEvents(evOut, machine)...)

	kept := entries[:0]
	for _, e := range entries {
		if !e.Time.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
	if len(kept) > maxOutputLines {
		kept = kept[len(kept)-maxOutputLines:]
	}
	return render(w, kept, jsonOut)
}

func render(w io.Writer, es []Entry, jsonOut bool) error {
	if jsonOut {
		if es == nil {
			es = []Entry{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(es)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tEVENT\tUSER\tDB\tCLIENT")
	for _, e := range es {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Time.UTC().Format("2006-01-02 15:04:05"),
			e.Event, dash(e.User), dash(e.DB), dash(e.Client))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// safe deja pasar solo un valor corto y sin espacios ni control: lo que llega
// del log lo controla el cliente (el nombre de usuario, por ejemplo).
func safe(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	var b strings.Builder
	for _, r := range s {
		if r > 32 && r < 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Prefijo del golden: log_line_prefix = '%m [%p] u=%u d=%d h=%h '.
var lineRe = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+) UTC \[\d+\] u=(\S*) d=(\S*) h=(\S*) (LOG|FATAL):  (.*)$`)

// ParseLog extrae las conexiones, desconexiones y autenticaciones fallidas.
// Ignora cualquier otra línea: no se copia texto libre del log.
func ParseLog(log string) []Entry {
	var es []Entry
	for _, line := range strings.Split(log, "\n") {
		m := lineRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05.999999999", m[1])
		if err != nil {
			continue
		}
		e := Entry{Time: t.UTC(), User: safe(m[2]), DB: safe(m[3]), Client: safe(m[4])}
		msg := m[6]
		switch {
		case m[5] == "LOG" && strings.HasPrefix(msg, "connection authorized:"):
			e.Event = "connect"
		case m[5] == "LOG" && strings.HasPrefix(msg, "disconnection:"):
			e.Event = "disconnect"
		case m[5] == "FATAL" && strings.HasPrefix(msg, "password authentication failed"):
			e.Event = "auth-failed"
		case m[5] == "FATAL" && strings.HasPrefix(msg, "no pg_hba.conf entry"):
			e.Event = "auth-rejected"
		default:
			continue
		}
		es = append(es, e)
	}
	return es
}

// ParseEvents filtra las líneas JSON de `kling events -json` de la máquina.
func ParseEvents(raw []byte, machine string) []Entry {
	var es []Entry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Time    time.Time `json:"time"`
			Type    string    `json:"type"`
			ID      string    `json:"id"`
			Name    string    `json:"name"`
			Message string    `json:"message"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || !strings.HasPrefix(ev.Type, "machine.") {
			continue
		}
		if ev.Name != machine && ev.ID != machine {
			continue
		}
		es = append(es, Entry{Time: ev.Time.UTC(), Event: eventName(ev.Type, ev.Message)})
	}
	return es
}

func eventName(typ, msg string) string {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "fork"):
		return "fork"
	case strings.Contains(low, "reset"):
		return "reset"
	}
	switch typ {
	case "machine.created", "machine.started":
		return "up"
	case "machine.frozen":
		return "freeze"
	case "machine.thawed":
		return "thaw"
	}
	return strings.TrimPrefix(typ, "machine.")
}
