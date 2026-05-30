// Command skillgen generates internal/skills/registry_gen.go from the skill
// bundles under internal/skills/<name>/skill.md. It is a thin wrapper around
// skills.GenerateRegistry so the codegen logic lives in one place that both this
// command and the drift test exercise.
//
// Invoked via the `go:generate` directive in internal/skills/loader.go.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/compshare-agent/internal/skills"
)

func main() {
	root := flag.String("root", "internal/skills", "skill bundle root directory")
	out := flag.String("out", "internal/skills/registry_gen.go", "generated file path")
	flag.Parse()

	src, err := skills.GenerateRegistry(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skillgen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "skillgen: write:", err)
		os.Exit(1)
	}
}
