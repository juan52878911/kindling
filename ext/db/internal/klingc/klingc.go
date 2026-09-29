// Package klingc es la costura entre kling db y el CLI kling.
package klingc

import (
	"context"
	"io"
)

type Kling interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) (stdout []byte, err error)
}
