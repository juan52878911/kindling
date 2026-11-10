package machine

import (
	"os"
	"strconv"
)

// lineaDeProceso es la línea de órdenes de pid tal como la ve liveVMs en
// Linux (/proc/<pid>/cmdline, con los NUL entre argumentos).
func lineaDeProceso(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	return string(b), err
}
