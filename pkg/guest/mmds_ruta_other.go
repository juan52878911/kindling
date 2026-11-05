//go:build !linux

package guest

import "errors"

// rutaNetlinkOS: fuera de Linux no hay netlink; SetupMMDSRoute cae a `ip`.
func rutaNetlinkOS([4]byte, string) error { return errors.ErrUnsupported }
