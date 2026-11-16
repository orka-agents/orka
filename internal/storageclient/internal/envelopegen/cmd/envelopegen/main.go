// Command envelopegen writes the storage client's write envelope decorators.
//
// Usage: envelopegen <package> <output file>
package main

import (
	"fmt"
	"os"

	"github.com/orka-agents/orka/internal/storageclient/internal/envelopegen"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: envelopegen <package> <output file>")
		os.Exit(2)
	}
	source, err := envelopegen.Generate(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "envelopegen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], source, 0o644); err != nil { //nolint:gosec // generated Go source is not secret
		fmt.Fprintln(os.Stderr, "envelopegen:", err)
		os.Exit(1)
	}
}
