//go:build !darwin

package main

// versionMacOS solo tiene sentido en macOS.
func versionMacOS() string { return "" }
