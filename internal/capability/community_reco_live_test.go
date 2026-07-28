//go:build live

// Diagnostic probe (not a gate): answers whether the community-image
// RECOMMENDATION turn ever sees the most-deployed images, or whether they are
// dropped before the model reads anything. It runs the production handler
// against the real API, then reports (a) the popularity ranking of the whole
// catalog, (b) which families survive into the rendered reply, and (c) which
// survive into the evidence envelope — the two things the model actually reads.
//
//	go test ./internal/capability -tags live -run TestLiveCommunityRecommendationReach -v \
//	    -reco-top-org <id> -reco-org <id> -reco-queries "数字人,,LiveTalking"
package capability

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/tools"
)

var (
	recoTopOrg  = flag.Uint("reco-top-org", 0, "top_organization_id (required)")
	recoOrg     = flag.Uint("reco-org", 0, "organization_id (required)")
	recoQueries = flag.String("reco-queries", "", "comma-separated queries; an empty element means no FuzzySearch")
	recoConfig  = flag.String("reco-config", "", "config.yaml path; default ../../deploy/conf/config.yaml")
)

// TestLiveCommunityFirstPageCoversTheHottest answers the precondition for the
// "attach the catalog's hottest families as server-side facts" option: a full
// catalog fetch is 9 pages / ~30s, far too slow for a hot path, so the cheap
// version can only afford ONE page. That is only worth building if page 1
// actually holds the most-deployed families — which is in doubt, because the
// upstream SortCondition{CreatedCount,ASC:false} looked like it was ignored.
//
//	go test ./internal/capability -tags live -run TestLiveCommunityFirstPageCoversTheHottest -v \
//	    -reco-top-org <id> -reco-org <id>
func TestLiveCommunityFirstPageCoversTheHottest(t *testing.T) {
	ctx, rt := recoLiveRuntime(t)

	full, err := imageExecuteAll(ctx, rt, communityImageAction, "CompshareImageGroup", map[string]any{})
	if err != nil {
		t.Fatalf("full catalog: %v", err)
	}
	type fam struct {
		name    string
		created int64
	}
	all := make([]fam, 0, 900)
	for _, g := range mapSliceAt(full, "CompshareImageGroup") {
		entry, ok := g.(map[string]any)
		if !ok {
			continue
		}
		all = append(all, fam{communityGroupName(entry), communityDeployCount(entry)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].created > all[j].created })
	t.Logf("full catalog: %d families", len(all))

	pages := map[string]map[string]any{
		"page1 plain":      {"Limit": 100, "Offset": 0},
		"page1 sorted":     {"Limit": 100, "Offset": 0, "SortCondition": map[string]any{"Field": "CreatedCount", "ASC": false}},
		"page1 no-readme":  {"Limit": 100, "Offset": 0, "ExcludeReadme": true},
		"page1 sorted+excl": {"Limit": 100, "Offset": 0, "ExcludeReadme": true,
			"SortCondition": map[string]any{"Field": "CreatedCount", "ASC": false}},
	}
	for label, args := range pages {
		raw, err := rt.Executor.Execute(ctx, communityImageAction, args)
		if err != nil {
			t.Logf("%-18s ERROR %v", label, err)
			continue
		}
		onPage := map[string]bool{}
		for _, g := range mapSliceAt(raw, "CompshareImageGroup") {
			if entry, ok := g.(map[string]any); ok {
				onPage[communityGroupName(entry)] = true
			}
		}
		hits := 0
		var missing []string
		for i, f := range all {
			if i >= 20 {
				break
			}
			if onPage[f.name] {
				hits++
			} else if len(missing) < 6 {
				missing = append(missing, fmt.Sprintf("#%d %s(%d)", i+1, f.name, f.created))
			}
		}
		t.Logf("%-18s rows=%d  global-top20 on page = %d/20  missing: %s",
			label, len(mapSliceAt(raw, "CompshareImageGroup")), hits, strings.Join(missing, " | "))
	}
}

// recoLiveRuntime builds the tenant context + real executor shared by the probes.
func recoLiveRuntime(t *testing.T) (context.Context, ReadRuntime) {
	t.Helper()
	if *recoTopOrg == 0 || *recoOrg == 0 {
		t.Skip("set -reco-top-org and -reco-org to run (real CompShare API)")
	}
	cfgPath := *recoConfig
	if cfgPath == "" {
		cfgPath = filepath.Join("..", "..", "deploy", "conf", "config.yaml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", cfgPath, err)
	}
	roleUrn := cfg.Agent.STS.DefaultRoleUrn
	if roleUrn == "" {
		roleUrn, err = tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, uint32(*recoTopOrg))
		if err != nil {
			t.Fatalf("role urn: %v", err)
		}
	}
	ctx := tools.WithUser(context.Background(), tools.UserContext{
		TopOrganizationID: uint32(*recoTopOrg),
		OrganizationID:    uint32(*recoOrg),
		CompanyID:         uint32(*recoTopOrg),
		RoleUrn:           roleUrn,
		SessionName:       cfg.Agent.STS.DefaultSessionName,
		ProjectId:         cfg.Agent.ProjectId,
		Region:            cfg.Agent.Region,
	})
	return ctx, ReadRuntime{Executor: recoExecutor{tools.NewExternalExecutor(cfg.Agent)}}
}

// recoExecutor adapts the real external executor to ReadExecutor. The image
// listing never takes the internal path, so ExecuteInternal forwards.
type recoExecutor struct{ *tools.ExternalExecutor }

func (e recoExecutor) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func TestLiveCommunityRecommendationReach(t *testing.T) {
	if *recoTopOrg == 0 || *recoOrg == 0 {
		t.Skip("set -reco-top-org and -reco-org to run (real CompShare API)")
	}
	cfgPath := *recoConfig
	if cfgPath == "" {
		cfgPath = filepath.Join("..", "..", "deploy", "conf", "config.yaml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", cfgPath, err)
	}
	roleUrn := cfg.Agent.STS.DefaultRoleUrn
	if roleUrn == "" {
		roleUrn, err = tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, uint32(*recoTopOrg))
		if err != nil {
			t.Fatalf("role urn: %v", err)
		}
	}
	ctx := tools.WithUser(context.Background(), tools.UserContext{
		TopOrganizationID: uint32(*recoTopOrg),
		OrganizationID:    uint32(*recoOrg),
		CompanyID:         uint32(*recoTopOrg),
		RoleUrn:           roleUrn,
		SessionName:       cfg.Agent.STS.DefaultSessionName,
		ProjectId:         cfg.Agent.ProjectId,
		Region:            cfg.Agent.Region,
	})
	rt := ReadRuntime{Executor: recoExecutor{tools.NewExternalExecutor(cfg.Agent)}}

	// A file (@path, one query per line, blank line = no FuzzySearch) avoids
	// pushing CJK through the shell's argument encoding.
	queries := []string{""}
	switch spec := *recoQueries; {
	case strings.HasPrefix(spec, "@"):
		body, err := os.ReadFile(strings.TrimPrefix(spec, "@"))
		if err != nil {
			t.Fatalf("read query file: %v", err)
		}
		normalized := strings.TrimRight(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
		queries = strings.Split(normalized, "\n")
	case spec != "":
		queries = strings.Split(spec, ",")
	}
	var out strings.Builder
	for _, q := range queries {
		q = strings.TrimSpace(q)
		label := q
		if label == "" {
			label = "(no FuzzySearch)"
		}
		fmt.Fprintf(&out, "\n================ query=%s ================\n", label)

		args := map[string]any{}
		if q != "" {
			args["FuzzySearch"] = q
		}
		raw, err := imageExecuteAll(ctx, rt, communityImageAction, "CompshareImageGroup", args)
		if err != nil {
			fmt.Fprintf(&out, "FETCH ERROR: %v\n", err)
			continue
		}
		groups := mapSliceAt(raw, "CompshareImageGroup")
		fmt.Fprintf(&out, "fetched groups=%d TotalCount=%v\n", len(groups), raw["TotalCount"])

		type row struct {
			name     string
			created  int64
			versions int
		}
		rows := make([]row, 0, len(groups))
		for _, g := range groups {
			entry, ok := g.(map[string]any)
			if !ok {
				continue
			}
			rows = append(rows, row{
				name:     communityGroupName(entry),
				created:  communityDeployCount(entry),
				versions: len(mapSliceAt(entry, "Data")),
			})
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].created > rows[j].created })
		fmt.Fprintf(&out, "--- top 20 families by 部署次数 (whole fetched catalog) ---\n")
		for i, r := range rows {
			if i >= 20 {
				break
			}
			fmt.Fprintf(&out, "%2d. %-42s created=%-7d versions=%d\n", i+1, r.name, r.created, r.versions)
		}

		// What the model reads #1: the rendered reply.
		reply := renderCommunityImageReply(raw, q, platform.ListMode(""))
		fmt.Fprintf(&out, "--- rendered reply (what lands in the tool observation) ---\n%s\n", reply)

		// What the model reads #2: the evidence envelope subjects.
		env := buildCommunityImageEnvelope(raw, q, platform.ListMode(""))
		fmt.Fprintf(&out, "--- envelope subjects (%d) ---\n", len(env.Subjects))
		for i, s := range env.Subjects {
			deploy := ""
			for _, f := range env.Facts {
				if f.SubjectID == s.ID && f.Key == "deploy_count" {
					deploy = fmt.Sprintf(" created=%v", f.Value)
				}
			}
			fmt.Fprintf(&out, "%2d. %s%s\n", i+1, s.Name, deploy)
		}
		for _, c := range env.Computed {
			fmt.Fprintf(&out, "computed %s = %v\n", c.Key, c.Value)
		}
	}

	text := out.String()
	t.Log(text)
	if path := os.Getenv("COMPSHARE_RECO_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
	}
}
