// Package routing loads deterministic route manifests. A route is a
// classify-then-dispatch entry for stable read-only console requests; it is not
// a body-read skill.
package routing

//go:generate go run github.com/compshare-agent/cmd/routegen --root . --out registry_gen.go

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// routeFS embeds every internal/routing/<name>/route.yaml so the generated
// registry remains available in deployed binaries without depending on cwd.
//
//go:embed */route.yaml
var routeFS embed.FS

const RouteFileName = "route.yaml"

const (
	VerificationProductionValidated = "production_validated"
	VerificationSpikeValidated      = "spike_validated"
	VerificationUnverified          = "unverified"
)

const ProvenanceHumanAuthored = "human_authored"

var routeNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*[a-z0-9]$`)

type PlannerExample struct {
	Question    string  `yaml:"question"`
	Confidence  float64 `yaml:"confidence"`
	ImageSource string  `yaml:"image_source,omitempty"`
	SearchQuery string  `yaml:"search_query,omitempty"`
	ListMode    string  `yaml:"list_mode,omitempty"`
	PriceKind   string  `yaml:"price_kind,omitempty"`
	CFSKind     string  `yaml:"cfs_kind,omitempty"`
	SizeGB      int     `yaml:"size_gb,omitempty"`
	Zone        string  `yaml:"zone,omitempty"`
	ChargeType  string  `yaml:"charge_type,omitempty"`
	DetailLevel string  `yaml:"detail_level,omitempty"`
}

type Route struct {
	Name              string
	Description       string
	IntentLabel       string
	RouteGroup        string
	RequiredTools     []string
	ToolSubset        []string
	RequiredCitation  bool
	HandlerKey        string
	PlannerDirectives []string
	PlannerExamples   []PlannerExample
	// AgentSlots declares the structured arguments accepted by the high-level
	// read capability generated from this route. It is a capability schema, not
	// a natural-language parser: adding a route-specific sentence matcher here is
	// intentionally impossible.
	AgentSlots         []string
	VerificationStatus string
	FieldRefsVerified  bool
	Provenance         string
	Path               string
}

type routeYAML struct {
	Name               string           `yaml:"name"`
	Description        string           `yaml:"description"`
	IntentLabel        string           `yaml:"intent_label"`
	RouteGroup         string           `yaml:"route_group"`
	RequiredTools      []string         `yaml:"required_tools"`
	ToolSubset         []string         `yaml:"tool_subset"`
	RequiredCitation   bool             `yaml:"required_citation"`
	HandlerKey         string           `yaml:"handler_key"`
	PlannerDirectives  []string         `yaml:"planner_directives"`
	PlannerExamples    []PlannerExample `yaml:"planner_examples"`
	AgentSlots         []string         `yaml:"agent_slots"`
	VerificationStatus string           `yaml:"verification_status"`
	FieldRefsVerified  *bool            `yaml:"field_refs_verified"`
	Provenance         string           `yaml:"provenance"`
}

type Loader struct {
	routes map[string]*Route
}

func NewLoader(root string) (*Loader, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("routing: read root %q: %w", root, err)
	}
	loaded := make(map[string]*Route)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), RouteFileName)
		if _, statErr := os.Stat(path); statErr != nil {
			continue
		}
		route, parseErr := ParseRouteFile(path)
		if parseErr != nil {
			return nil, fmt.Errorf("routing: load %q: %w", path, parseErr)
		}
		if _, dup := loaded[route.Name]; dup {
			return nil, fmt.Errorf("routing: duplicate route name %q", route.Name)
		}
		loaded[route.Name] = route
	}
	return &Loader{routes: loaded}, nil
}

func ParseRouteFile(path string) (*Route, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read route file: %w", err)
	}
	var raw routeYAML
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}
	if raw.Name == "" {
		return nil, fmt.Errorf("name must be non-empty")
	}
	if len(raw.Name) > 64 || !routeNameRE.MatchString(raw.Name) {
		return nil, fmt.Errorf("name %q must match [a-z][a-z0-9_]*[a-z0-9] and be 1-64 chars", raw.Name)
	}
	if dir := filepath.Base(filepath.Dir(path)); raw.Name != dir {
		return nil, fmt.Errorf("name %q must equal directory name %q", raw.Name, dir)
	}
	if raw.Description == "" {
		return nil, fmt.Errorf("route %q: description must be non-empty", raw.Name)
	}
	if raw.IntentLabel == "" {
		return nil, fmt.Errorf("route %q: intent_label must be non-empty", raw.Name)
	}
	if raw.RouteGroup == "" {
		return nil, fmt.Errorf("route %q: route_group must be non-empty", raw.Name)
	}
	if len(raw.RequiredTools) == 0 {
		return nil, fmt.Errorf("route %q: required_tools must be non-empty", raw.Name)
	}
	if len(raw.ToolSubset) == 0 {
		return nil, fmt.Errorf("route %q: tool_subset must be non-empty", raw.Name)
	}
	if raw.HandlerKey == "" {
		return nil, fmt.Errorf("route %q: handler_key must be non-empty", raw.Name)
	}
	knownAgentSlots := map[string]struct{}{
		"target_refs": {}, "metrics": {}, "time_window": {}, "image_source": {},
		"search_query": {}, "list_mode": {}, "price_kind": {}, "gpu_count": {}, "cfs_kind": {},
		"size_gb": {}, "zone": {}, "charge_type": {}, "detail_level": {},
	}
	seenAgentSlots := make(map[string]struct{}, len(raw.AgentSlots))
	for _, slot := range raw.AgentSlots {
		if _, ok := knownAgentSlots[slot]; !ok {
			return nil, fmt.Errorf("route %q: unknown agent slot %q", raw.Name, slot)
		}
		if _, duplicate := seenAgentSlots[slot]; duplicate {
			return nil, fmt.Errorf("route %q: duplicate agent slot %q", raw.Name, slot)
		}
		seenAgentSlots[slot] = struct{}{}
	}
	switch raw.VerificationStatus {
	case VerificationProductionValidated, VerificationSpikeValidated, VerificationUnverified:
	default:
		return nil, fmt.Errorf("route %q: verification_status %q must be one of production_validated|spike_validated|unverified", raw.Name, raw.VerificationStatus)
	}
	if raw.FieldRefsVerified == nil {
		return nil, fmt.Errorf("route %q: field_refs_verified must be explicitly set", raw.Name)
	}
	for i, ex := range raw.PlannerExamples {
		if ex.Question == "" {
			return nil, fmt.Errorf("route %q: planner_examples[%d].question must be non-empty", raw.Name, i)
		}
		if ex.Confidence < 0 || ex.Confidence > 1 {
			return nil, fmt.Errorf("route %q: planner_examples[%d].confidence must be in [0,1]", raw.Name, i)
		}
	}
	return &Route{
		Name:               raw.Name,
		Description:        raw.Description,
		IntentLabel:        raw.IntentLabel,
		RouteGroup:         raw.RouteGroup,
		RequiredTools:      raw.RequiredTools,
		ToolSubset:         raw.ToolSubset,
		RequiredCitation:   raw.RequiredCitation,
		HandlerKey:         raw.HandlerKey,
		PlannerDirectives:  raw.PlannerDirectives,
		PlannerExamples:    raw.PlannerExamples,
		AgentSlots:         append([]string(nil), raw.AgentSlots...),
		VerificationStatus: raw.VerificationStatus,
		FieldRefsVerified:  *raw.FieldRefsVerified,
		Provenance:         raw.Provenance,
		Path:               filepath.ToSlash(path),
	}, nil
}

func (l *Loader) Fetch(name string) (*Route, bool) {
	r, ok := l.routes[name]
	return r, ok
}

func (l *Loader) Len() int { return len(l.routes) }

func (l *Loader) Names() []string {
	names := make([]string, 0, len(l.routes))
	for name := range l.routes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
