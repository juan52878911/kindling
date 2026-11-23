// Package dorado fabrica el dorado de un backend de modelo (plan de la ronda 8,
// §1.3): arranca la imagen, sondea, calienta, aprieta y congela.
package dorado

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	LabelModel   = "dorado.model"
	LabelVersion = "dorado.version"
)

type Client interface {
	Run(ctx context.Context, r api.RunRequest) (*api.Machine, error)
	Guest(ctx context.Context, ref string, r api.GuestRequest) (*api.GuestResponse, error)
	Squeeze(ctx context.Context, ref string) (*api.SqueezeResult, error)
	Commit(ctx context.Context, ref, name string, replace bool) (*api.Snapshot, error)
	Remove(ctx context.Context, ref string) error
}

type Probe struct {
	Path    string
	Expect  int
	Timeout time.Duration
}

type Request struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type Options struct {
	Image, Snapshot string
	Port            int
	VCPUs, MemMiB   int
	CPUPct          int
	Health          Probe
	Warmup          []Request
	Env             []string
	AllowEnv        bool
	Shares          []api.ShareSpec
	Labels          map[string]string
	AllowExec       bool
	Replace         bool
	Wait            time.Duration
	Ready           func(ctx context.Context, ref string, timeout time.Duration) error
	AfterWarmup     func(ctx context.Context, ref string) error
	Log             func(format string, args ...any)
}

type Result struct {
	Snapshot *api.Snapshot
	Machine  string
	BootMS   int64
	LoadTime time.Duration
	Warmups  int
}

func Make(ctx context.Context, c Client, o Options) (*Result, error) {
	return nil, errors.New("not implemented")
}

func ParseWarmup(r io.Reader) ([]Request, error) { return nil, errors.New("not implemented") }

func Versioned(model string, n int) string { return "" }

func ParseVersion(model, snapshot string) (int, bool) { return 0, false }

const maxWarmup = 1000

func (r Request) bodyText() string { return "" }

func NextVersion(model string, snapshots []string) int { return 0 }
