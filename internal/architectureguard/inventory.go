package architectureguard

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type MigrationInventory struct {
	Version        int             `json:"version"`
	BaselineCommit string          `json:"baseline_commit"`
	PreAgentExits  []InventorySite `json:"pre_agent_exits"`
	SemanticOwners []InventorySite `json:"semantic_owners"`
}

type InventorySite struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Symbol      string `json:"symbol"`
	Needle      string `json:"needle"`
	Category    string `json:"category"`
	CurrentRole string `json:"current_role"`
	Target      string `json:"target"`
	Phase       string `json:"phase"`
}

func LoadInventory(path string) (MigrationInventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MigrationInventory{}, err
	}
	var inventory MigrationInventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return MigrationInventory{}, err
	}
	return inventory, nil
}

func ValidateInventory(root string, inventory MigrationInventory) error {
	if inventory.Version != 1 || strings.TrimSpace(inventory.BaselineCommit) == "" {
		return fmt.Errorf("migration inventory header is incomplete")
	}
	validCategories := map[string]bool{
		"protocol": true, "security": true, "confirmation": true, "business_direct": true,
		"parse_failure": true, "quota": true, "semantic_owner": true,
	}
	seen := map[string]bool{}
	all := append(append([]InventorySite(nil), inventory.PreAgentExits...), inventory.SemanticOwners...)
	for _, site := range all {
		if site.ID == "" || site.Path == "" || site.Symbol == "" || site.Category == "" ||
			site.CurrentRole == "" || site.Target == "" || site.Phase == "" {
			return fmt.Errorf("inventory entry %q is incomplete", site.ID)
		}
		if seen[site.ID] {
			return fmt.Errorf("duplicate inventory id %q", site.ID)
		}
		seen[site.ID] = true
		if !validCategories[site.Category] {
			return fmt.Errorf("inventory entry %q has unknown category %q", site.ID, site.Category)
		}
		if err := validateSite(root, site); err != nil {
			return fmt.Errorf("inventory entry %q: %w", site.ID, err)
		}
	}
	if len(inventory.PreAgentExits) < 10 || len(inventory.SemanticOwners) < 8 {
		return fmt.Errorf("migration inventory is implausibly small: exits=%d owners=%d", len(inventory.PreAgentExits), len(inventory.SemanticOwners))
	}
	return nil
}

func validateSite(root string, site InventorySite) error {
	path := filepath.Join(root, filepath.FromSlash(site.Path))
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return err
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != site.Symbol {
			continue
		}
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		if start < 0 || end > len(data) || start >= end {
			return fmt.Errorf("cannot slice symbol %s", site.Symbol)
		}
		if site.Needle != "" && !strings.Contains(string(data[start:end]), site.Needle) {
			return fmt.Errorf("symbol %s no longer contains %q; update or remove the reviewed entry", site.Symbol, site.Needle)
		}
		return nil
	}
	return fmt.Errorf("symbol %s not found in %s", site.Symbol, site.Path)
}
