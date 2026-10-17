// Command dlm is the dlm-server daemon and command-line client.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if _, err := fmt.Fprintf(os.Stdout, "dlm %s\n", version); err != nil {
		os.Exit(1)
	}
}
