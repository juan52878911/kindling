package guest

import (
	"fmt"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// Los números de RLIMIT_* de Linux (iguales en amd64 y arm64; syscall no
// exporta todos).
var rlimitNum = map[string]int{
	"cpu": 0, "fsize": 1, "data": 2, "stack": 3, "core": 4, "rss": 5,
	"nproc": 6, "nofile": 7, "memlock": 8, "as": 9, "locks": 10,
	"sigpending": 11, "msgqueue": 12, "nice": 13, "rtprio": 14,
}

func setRlimit(u api.Ulimit) error {
	n, ok := rlimitNum[u.Name]
	if !ok {
		return fmt.Errorf("unknown resource")
	}
	return syscall.Setrlimit(n, &syscall.Rlimit{Cur: u.Soft, Max: u.Hard})
}
