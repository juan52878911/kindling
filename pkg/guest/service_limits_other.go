//go:build !linux

package guest

import (
	"errors"

	"github.com/juan52878911/kindling/pkg/api"
)

func setRlimit(api.Ulimit) error { return errors.New("only on Linux") }
