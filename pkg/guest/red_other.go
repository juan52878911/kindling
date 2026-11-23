//go:build !linux

package guest

import "errors"

// configurarRedOS: fuera de Linux no hay netlink.
func configurarRedOS(string) error { return errors.ErrUnsupported }
