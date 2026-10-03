// Command refhub is the reference STUB hub used only to validate the
// acceptance suite. It is not harness-hub.
package main

import (
	"os"

	"github.com/RCX1t7/plexus/tests/refhub"
)

func main() { os.Exit(refhub.Main(os.Args[1:])) }
