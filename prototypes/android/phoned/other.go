//go:build !linux

package main

import "errors"

// kling-phoned solo corre dentro de la microVM (Linux). En otros sistemas el
// paquete compila (go vet ./..., las pruebas de lo que no es Linux) y nada más.

var errLinuxOnly = errors.New("kling-phoned runs only inside the Linux guest")

func runDaemon() error { return errLinuxOnly }
func runStage2() error { return errLinuxOnly }
func runReady() int    { println(errLinuxOnly.Error()); return 1 }
func runIdentity() int { println(errLinuxOnly.Error()); return 1 }
