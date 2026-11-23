package main

import (
	"context"
	"errors"
	"time"

	"github.com/juan52878911/kindling/internal/aigw"
	"github.com/juan52878911/kindling/internal/dorado"
	"github.com/juan52878911/kindling/pkg/api"
)

type modelDeployOptions struct {
	Name, Image   string
	Port          int
	Contract      string
	Routes        []string
	Health        aigw.HealthConfig
	Warmup        []dorado.Request
	VCPUs, MemMiB int
	CPUPct        int
	Idle          string
	MaxInflight   int
	MaxReplicas   int
	Env           []string
	AllowEnv      bool
	AllowExec     bool
	Wait          time.Duration
	Config        string
}

type modelDeployResult struct {
	Snapshot string
	Version  int
	Written  bool
	Golden   *dorado.Result
}

func modelDeploy(ctx context.Context, c *api.Client, o modelDeployOptions) (*modelDeployResult, error) {
	return nil, errors.New("not implemented")
}
