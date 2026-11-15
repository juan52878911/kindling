//go:build !linux

package net

func direccionesHost() ([]DireccionHost, error) { return direccionesPorInterfaz() }
