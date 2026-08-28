package architectureguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const BaselineVersion = 1

type Finding struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Symbol  string `json:"symbol"`
	Callee  string `json:"callee,omitempty"`
	Value   string `json:"value,omitempty"`
	Ordinal int    `json:"ordinal,omitempty"`
}

type Baseline struct {
	Version  int       `json:"version"`
	Findings []Finding `json:"findings"`
}

var semanticSymbolName = regexp.MustCompile(`(?i)(fromUserText|infer.*Action|resolveContextDecision|contextDecision|taskSlot|should.*Direct|tryDirect|tryPlannerDispatch|dispatchToolScope|specForIntent|plannedExecutionPathForIntent|intentToolSubset)`)

func FindRepoRoot(start string) (string, error) {
	root, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root, nil
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("go.mod not found above %s", start)
		}
		root = parent
	}
}

func Scan(root string) (Baseline, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Baseline{}, err
	}
	fset := token.NewFileSet()
	findings := make([]Finding, 0, 512)
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") ||
			strings.HasPrefix(rel, "internal/architectureguard/") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		for _, decl := range file.Decls {
			switch node := decl.(type) {
			case *ast.FuncDecl:
				if semanticSymbolName.MatchString(node.Name.Name) {
					findings = append(findings, Finding{Kind: "semantic_symbol", Path: rel, Symbol: node.Name.Name})
				}
				if node.Body != nil {
					scanCalls(rel, node.Name.Name, node.Body, &findings)
				}
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if ok && semanticSymbolName.MatchString(typeSpec.Name.Name) {
						findings = append(findings, Finding{Kind: "semantic_type", Path: rel, Symbol: typeSpec.Name.Name})
					}
				}
				scanCalls(rel, "init", node, &findings)
			}
		}
		return nil
	})
	if err != nil {
		return Baseline{}, err
	}
	return Baseline{Version: BaselineVersion, Findings: stableFindings(findings)}, nil
}

func scanCalls(path, symbol string, node ast.Node, findings *[]Finding) {
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		callee := pkg.Name + "." + selector.Sel.Name
		kind := ""
		switch callee {
		case "regexp.Compile", "regexp.MustCompile", "regexp.CompilePOSIX", "regexp.MustCompilePOSIX":
			kind = "regex"
		case "strings.Contains", "strings.HasPrefix", "strings.HasSuffix",
			"strings.Index", "strings.IndexAny", "strings.LastIndex":
			kind = "string_heuristic"
		}
		if kind == "" {
			return true
		}
		// Literal provenance is not semantic interpretation. Keep one explicit,
		// reviewable primitive for source-span proof; every other Contains/Index
		// site remains forbidden unless present in the reviewed baseline.
		if path == "internal/platform/provenance.go" && symbol == "ContainsLiteralSpan" && callee == "strings.Contains" {
			return true
		}
		value := ""
		if len(call.Args) > 0 {
			if literal, ok := call.Args[len(call.Args)-1].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				value, _ = strconv.Unquote(literal.Value)
			}
		}
		*findings = append(*findings, Finding{Kind: kind, Path: path, Symbol: symbol, Callee: callee, Value: value})
		return true
	})
}

func stableFindings(in []Finding) []Finding {
	sort.Slice(in, func(i, j int) bool { return findingKey(in[i], false) < findingKey(in[j], false) })
	seen := map[string]int{}
	for i := range in {
		base := findingKey(in[i], false)
		seen[base]++
		in[i].Ordinal = seen[base]
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", base, in[i].Ordinal)))
		in[i].ID = hex.EncodeToString(sum[:8])
		if in[i].Ordinal == 1 {
			in[i].Ordinal = 0
		}
	}
	return in
}

func findingKey(f Finding, includeOrdinal bool) string {
	parts := []string{f.Kind, f.Path, f.Symbol, f.Callee, f.Value}
	if includeOrdinal {
		parts = append(parts, strconv.Itoa(f.Ordinal))
	}
	return strings.Join(parts, "\x00")
}

func Unexpected(current, reviewed Baseline) []Finding {
	allowed := make(map[string]struct{}, len(reviewed.Findings))
	for _, finding := range reviewed.Findings {
		allowed[findingKey(finding, true)] = struct{}{}
	}
	var out []Finding
	for _, finding := range current.Findings {
		if _, ok := allowed[findingKey(finding, true)]; !ok {
			out = append(out, finding)
		}
	}
	return out
}

func LoadBaseline(path string) (Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Baseline{}, err
	}
	var baseline Baseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return Baseline{}, err
	}
	if baseline.Version != BaselineVersion {
		return Baseline{}, fmt.Errorf("unsupported architecture baseline version %d", baseline.Version)
	}
	return baseline, nil
}

func WriteBaseline(path string, baseline Baseline) error {
	data, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
