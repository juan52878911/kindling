package main

import (
	"context"
	"errors"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

type rollOptions struct {
	Model    string
	To       int
	Config   string
	NoReload bool
	Drain    time.Duration
	Poll     time.Duration
}

type rollResult struct {
	From, To string
	Removed  []string
}

func modelRoll(ctx context.Context, c *api.Client, reload func(context.Context) error, o rollOptions) (*rollResult, error) {
	return nil, errors.New("not implemented")
}
