package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/compshare-agent/internal/architectureguard"
)

func main() {
	rootFlag := flag.String("root", ".", "repository root or a path below it")
	baselineFlag := flag.String("baseline", "internal/architectureguard/baseline.json", "reviewed maximum set")
	write := flag.Bool("write", false, "write the current reviewed baseline")
	flag.Parse()

	root, err := architectureguard.FindRepoRoot(*rootFlag)
	fatal(err)
	current, err := architectureguard.Scan(root)
	fatal(err)
	baselinePath := *baselineFlag
	if !filepath.IsAbs(baselinePath) {
		baselinePath = filepath.Join(root, filepath.FromSlash(baselinePath))
	}
	if *write {
		fatal(architectureguard.WriteBaseline(baselinePath, current))
		fmt.Printf("wrote reviewed architecture baseline: %d findings\n", len(current.Findings))
		return
	}
	reviewed, err := architectureguard.LoadBaseline(baselinePath)
	fatal(err)
	unexpected := architectureguard.Unexpected(current, reviewed)
	if len(unexpected) > 0 {
		for _, finding := range unexpected {
			fmt.Fprintf(os.Stderr, "%s %s %s %s %q\n", finding.Kind, finding.Path, finding.Symbol, finding.Callee, finding.Value)
		}
		fatal(fmt.Errorf("found %d unreviewed semantic patch sites", len(unexpected)))
	}
	fmt.Printf("architecture audit ok: %d current reviewed findings (deletions allowed, additions forbidden)\n", len(current.Findings))
}

func fatal(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
