package intent

// Planner one-shot examples — disk-backed migration (C5 Phase A).
//
// Background
// ----------
// plannerPromptExampleGroups() in planner.go inlines ~30 one-shot
// examples across 7 intent groups. That data is editorial — adding /
// editing a one-shot example is a planner-prompt change, not a
// behavioral code change. Mixing the two in a Go literal block makes
// editorial review heavy (need to know Go) and obscures which intent
// owns which example.
//
// Phase A (this PR, task #86): introduce the disk-backed loader and
// migrate ONE intent group (diagnosis, 1 example) as proof. The byte-
// equal contract below pins that the rendered planner prompt is
// identical pre/post migration — moving an example to disk must NOT
// alter the system prompt string. Future Phase B/C/... migrate
// remaining intents (knowledge_qa, billing_*, monitor_*, etc.) one
// per PR, gated by the same byte-equal test extended each round.
//
// Why not migrate everything at once
// ----------------------------------
// ds-v4-flash classification is sensitive to prompt ordering and
// whitespace. A single-PR migration of 30 examples is too large to
// review confidently and too coarse to roll back if a regression
// surfaces. Per-intent slices keep the blast radius small and the
// CLI eval matrix tractable.
//
// File format
// -----------
// planner_examples/<intent>.md with YAML frontmatter:
//
//   ---
//   intent: <intent_label>
//   source: "<source description>"
//   examples:
//     - question: "<user question>"
//       plan_json: '<full JSON plan as a string>'
//       source: "<example source description>"
//   ---
//
// Body (markdown after closing ---) is documentation only; not parsed.

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed planner_examples/*.md
var plannerExamplesFS embed.FS

// plannerExampleFile is the on-disk frontmatter shape. Mapped 1:1 to
// the in-memory plannerPromptExampleGroup struct.
type plannerExampleFile struct {
	Intent   string                `yaml:"intent"`
	Source   string                `yaml:"source"`
	Examples []plannerExampleEntry `yaml:"examples"`
}

type plannerExampleEntry struct {
	Question string `yaml:"question"`
	PlanJSON string `yaml:"plan_json"`
	Source   string `yaml:"source"`
}

// diskPlannerExampleGroups returns the disk-backed example groups
// indexed by Intent. Used by plannerPromptExampleGroups() to splice
// migrated intents into the prompt in place of inline literals.
//
// Phase A only migrates IntentDiagnosis. Other intents return zero
// entries here and remain inline in planner.go; the byte-equal test
// (planner_examples_test.go) pins that the rendered prompt is
// identical before and after.
var diskPlannerExampleGroups = mustLoadPlannerExampleGroups()

func mustLoadPlannerExampleGroups() map[Intent]plannerPromptExampleGroup {
	loaded, err := loadPlannerExampleGroups(plannerExamplesFS)
	if err != nil {
		panic(fmt.Sprintf("intent: planner example load failed: %v", err))
	}
	return loaded
}

func loadPlannerExampleGroups(efs fs.FS) (map[Intent]plannerPromptExampleGroup, error) {
	entries, err := fs.ReadDir(efs, "planner_examples")
	if err != nil {
		return nil, fmt.Errorf("read planner_examples dir: %w", err)
	}
	out := map[Intent]plannerPromptExampleGroup{}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, "_") || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := fs.ReadFile(efs, "planner_examples/"+name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		group, err := parsePlannerExampleFrontmatter(data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		if _, dup := out[group.Intent]; dup {
			return nil, fmt.Errorf("duplicate planner_examples entry for intent %q (file %s)", group.Intent, name)
		}
		out[group.Intent] = group
	}
	return out, nil
}

func parsePlannerExampleFrontmatter(data []byte) (plannerPromptExampleGroup, error) {
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return plannerPromptExampleGroup{}, fmt.Errorf("missing frontmatter `---` opener")
	}
	rest := strings.TrimPrefix(content, "---")
	rest = strings.TrimLeft(rest, "\r\n")
	closer := strings.Index(rest, "\n---")
	if closer < 0 {
		return plannerPromptExampleGroup{}, fmt.Errorf("missing frontmatter `---` closer")
	}
	frontmatter := rest[:closer]
	var file plannerExampleFile
	decoder := yaml.NewDecoder(bytes.NewReader([]byte(frontmatter)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return plannerPromptExampleGroup{}, fmt.Errorf("yaml unmarshal: %w", err)
	}
	if file.Intent == "" {
		return plannerPromptExampleGroup{}, fmt.Errorf("intent must be non-empty")
	}
	if file.Source == "" {
		return plannerPromptExampleGroup{}, fmt.Errorf("source must be non-empty")
	}
	if len(file.Examples) == 0 {
		return plannerPromptExampleGroup{}, fmt.Errorf("examples must be non-empty")
	}
	group := plannerPromptExampleGroup{
		Intent: Intent(file.Intent),
		Source: file.Source,
	}
	for i, ex := range file.Examples {
		if strings.TrimSpace(ex.Question) == "" {
			return plannerPromptExampleGroup{}, fmt.Errorf("examples[%d].question must be non-empty", i)
		}
		if strings.TrimSpace(ex.PlanJSON) == "" {
			return plannerPromptExampleGroup{}, fmt.Errorf("examples[%d].plan_json must be non-empty", i)
		}
		if strings.TrimSpace(ex.Source) == "" {
			return plannerPromptExampleGroup{}, fmt.Errorf("examples[%d].source must be non-empty", i)
		}
		group.Examples = append(group.Examples, plannerPromptExample{
			Question: ex.Question,
			PlanJSON: ex.PlanJSON,
			Source:   ex.Source,
		})
	}
	return group, nil
}
