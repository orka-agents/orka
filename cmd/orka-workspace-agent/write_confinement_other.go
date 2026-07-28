//go:build !linux

package main

import "fmt"

func commandWriteConfinementSupported() bool {
	return false
}

func runWriteConfinedCommand(_ []string) error {
	return fmt.Errorf("command write confinement is unsupported on this platform")
}
