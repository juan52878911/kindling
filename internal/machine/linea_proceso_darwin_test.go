package machine

// lineaDeProceso es la línea de órdenes de pid tal como la ve liveVMs en
// macOS (kern.procargs2).
func lineaDeProceso(pid int) (string, error) {
	return lineaDeOrdenes(int32(pid), make([]byte, procArgsMax))
}
