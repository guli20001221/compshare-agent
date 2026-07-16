package capability

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCapabilityDoesNotUseIntentHandlerMachinery is the migration-discipline
// gate: a migrated read capability owns its typed vertical and must not reach
// back into the legacy intent router's request-shaped machinery. The capability
// package may still name intent for the Intent identity enum and the
// not-yet-migrated legacy request-type aliases, but no production file may USE
// intent.Slots, intent.HandlerRequest, intent.HandlerResult, intent.DispatchRoute
// or the typedHandlerRequest bridge. It parses the AST (ignoring comments) so a
// doc comment mentioning these concepts is fine — only real code usage fails.
func TestCapabilityDoesNotUseIntentHandlerMachinery(t *testing.T) {
	forbiddenSelectors := map[string]bool{
		"Slots":          true,
		"HandlerRequest": true,
		"HandlerResult":  true,
		"DispatchRoute":  true,
	}
	forbiddenIdents := map[string]bool{"typedHandlerRequest": true}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		ast.Inspect(parsed, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := node.X.(*ast.Ident); ok && id.Name == "intent" && forbiddenSelectors[node.Sel.Name] {
					t.Errorf("%s uses intent.%s — a migrated read capability owns its typed vertical (P3 gate)", file, node.Sel.Name)
				}
			case *ast.Ident:
				if forbiddenIdents[node.Name] {
					t.Errorf("%s uses %s — the legacy Slots bridge must not appear in capability (P3 gate)", file, node.Name)
				}
			}
			return true
		})
	}
}
