// Command routegen generates internal/routing/registry_gen.go from the route
// manifests under internal/routing/<name>/route.yaml.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/compshare-agent/internal/routing"
)

func main() {
	root := flag.String("root", "internal/routing", "route manifest root directory")
	out := flag.String("out", "internal/routing/registry_gen.go", "generated file path")
	flag.Parse()

	src, err := routing.GenerateRegistry(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "routegen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "routegen: write:", err)
		os.Exit(1)
	}
}
