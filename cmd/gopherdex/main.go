// Command gopherdex signs in to a Gopherdex registry and publishes Go modules
// from git tags.
//
//	gopherdex login --registry https://gopherdex.dev
//	git tag v1.0.0
//	gopherdex publish
package main

import (
	"os"

	"github.com/parthiban-sivakumar/gopherdex/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.OSEnv()))
}
