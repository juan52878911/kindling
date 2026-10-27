package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// warmFake apunta cada llamada, en orden, y falla donde se le diga.
type warmFake struct {
	calls     []string
	ready     string
	failExec  int // nº (desde 1) del paso que sale con 1; 0 = ninguno
	execs     int
	run       api.RunRequest
	commit    api.CommitRequest
	removeCtx []error
}

func (f *warmFake) Run(_ context.Context, r api.RunRequest) (*api.Machine, error) {
	f.calls = append(f.calls, "run")
	f.run = r
	ready := f.ready
	if ready == "" {
		ready = api.ReadyYes
	}
	return &api.Machine{Name: r.Name, Ready: ready}, nil
}

func (f *warmFake) Exec(_ context.Context, ref string, r api.ExecRequest, on func(string, []byte) error) (*api.ExecResult, error) {
	f.execs++
	f.calls = append(f.calls, "exec:"+strings.Join(r.Cmd, " "))
	_ = on("stdout", []byte("out "+r.Cmd[2]+"\n"))
	if f.failExec == f.execs {
		return &api.ExecResult{ExitCode: 1}, nil
	}
	return &api.ExecResult{}, nil
}

func (f *warmFake) CommitWith(_ context.Context, ref string, r api.CommitRequest) (*api.Snapshot, error) {
	f.calls = append(f.calls, "commit:"+ref)
	f.commit = r
	return &api.Snapshot{Name: r.Name}, nil
}

func (f *warmFake) Remove(ctx context.Context, ref string) error {
	f.calls = append(f.calls, "rm:"+ref)
	f.removeCtx = append(f.removeCtx, ctx.Err())
	return nil
}

func testWarmSpec() warmSpec {
	return warmFrom("pgv", "", []string{"a", "b"}, 0, 0, "internet", time.Minute, 5*time.Minute, true)
}

func TestWarmUpEncadena(t *testing.T) {
	f := &warmFake{}
	var out bytes.Buffer
	snap, err := warmUp(context.Background(), f, testWarmSpec(), &out, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "exec:sh -c a", "exec:sh -c b", "commit:pgv-warm", "rm:pgv-warm"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if snap.Name != "pgv" {
		t.Errorf("template = %q, want pgv (the image's name)", snap.Name)
	}
	// CPU completa de todas las vCPU, exec permitido y espera a "listo".
	r := f.run
	if r.CPUPct != 200 || r.VCPUs != 2 || r.MemMiB != 1024 || !r.AllowExec || !r.WaitReady || r.Egress != "internet" {
		t.Errorf("run = %+v", r)
	}
	if !f.commit.Replace || f.commit.SkipReady || f.commit.ReadyTimeoutSeconds != 60 {
		t.Errorf("commit = %+v", f.commit)
	}
	if out.String() != "out a\nout b\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestWarmUpPasoQueFallaAbortaYRetira(t *testing.T) {
	f := &warmFake{failExec: 1}
	_, err := warmUp(context.Background(), f, testWarmSpec(), &bytes.Buffer{}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "step 1 exited with status 1") {
		t.Fatalf("err = %v", err)
	}
	// Ni segundo paso ni plantilla; la máquina de preparación se retira.
	want := []string{"run", "exec:sh -c a", "rm:pgv-warm"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestWarmUpNoListo(t *testing.T) {
	f := &warmFake{ready: api.ReadyFailed}
	_, err := warmUp(context.Background(), f, testWarmSpec(), &bytes.Buffer{}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"run", "rm:pgv-warm"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestWarmUpRetiraAunqueSeCancele(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &warmFake{}
	g := &cancelOnExec{warmFake: f, cancel: cancel}
	_, err := warmUp(ctx, g, testWarmSpec(), &bytes.Buffer{}, func(string, ...any) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.removeCtx); n != 1 || f.removeCtx[0] != nil {
		t.Errorf("remove contexts = %v: the cleanup must not use the cancelled one", f.removeCtx)
	}
}

type cancelOnExec struct {
	*warmFake
	cancel func()
}

func (c *cancelOnExec) Exec(ctx context.Context, ref string, r api.ExecRequest, on func(string, []byte) error) (*api.ExecResult, error) {
	c.cancel()
	return nil, ctx.Err()
}
