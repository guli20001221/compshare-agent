package engine

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// p3TraceReplayCase intentionally contains only a one-way trace identifier and
// an action family. See eval/toolworkflow/README.md: raw production records,
// prompts and tool arguments remain ignored under eval/reports/.
type p3TraceReplayCase struct {
	IDHash   string `json:"id_hash"`
	Export   string `json:"export"`
	Category string `json:"category"`
	Source   string `json:"source"`
}

var p3ReplayCategoryTargets = []struct {
	category string
	limit    int
	actions  []string
}{
	{category: "shared_visibility", limit: 3, actions: []string{"DescribeCompShareSharingImages"}},
	{category: "custom_image_create", limit: 3, actions: []string{"CreateCompShareCustomImage", "CreateCustomImageWorkflow"}},
	{category: "custom_image_catalog", limit: 6, actions: []string{"DescribeCompShareCustomImages"}},
	{category: "community_catalog", limit: 15, actions: []string{"DescribeCommunityImages"}},
	{category: "platform_catalog", limit: 26, actions: []string{"DescribeCompShareImages"}},
}

func TestP3ToolWindowHistoricalActionReplay(t *testing.T) {
	cases := loadP3TraceReplayCases(t)
	require.Len(t, cases, 53)

	wantCounts := map[string]int{
		"platform_catalog":     26,
		"community_catalog":    15,
		"custom_image_catalog": 6,
		"custom_image_create":  3,
		"shared_visibility":    3,
	}
	gotCounts := map[string]int{}
	seen := map[string]struct{}{}
	for _, c := range cases {
		require.Equal(t, "agent_trace", c.Source)
		require.NotEmpty(t, c.Export)
		_, err := hex.DecodeString(c.IDHash)
		require.NoError(t, err)
		require.Len(t, c.IDHash, 64)
		_, duplicate := seen[c.IDHash]
		require.False(t, duplicate, "hash must identify exactly one replay event")
		seen[c.IDHash] = struct{}{}
		gotCounts[c.Category]++

		switch c.Category {
		case "platform_catalog", "community_catalog", "custom_image_catalog", "shared_visibility":
			// A catalog call is browsing evidence, not a user-confirmed source.
			// Keeping the full window preserves platform+community recommendation
			// and supported custom/shared reinstall follow-ups.
			assertHistoricalCatalogKeepsFullWindow(t, c.Category)
		case "custom_image_create":
			scope, ok := workflowToolScope("CreateCustomImageWorkflow")
			require.True(t, ok)
			assert.Equal(t, tools.ToolScopeNamed, scope.Mode)
			assert.Equal(t, scopeReasonCreateCustomImage, scope.Reason)
			want := append([]string(nil), centralAgentToolNames(false, true)...)
			want = append(want, "RequestCreateCustomImage")
			assert.ElementsMatch(t, want, scope.Names)
		default:
			t.Fatalf("unexpected replay category %q", c.Category)
		}
	}
	assert.Equal(t, wantCounts, gotCounts)
}

// TestP3ToolWindowHistoricalActionReplaySourceVerification lets a maintainer
// prove the committed, privacy-safe manifest still comes from the ignored raw
// exports. It is intentionally opt-in: CI and contributors without access to
// production exports run the manifest replay above, while a local audit sets
// COMPSHARE_P3_RAW_REPORT_ROOT to the directory containing the two exports.
func TestP3ToolWindowHistoricalActionReplaySourceVerification(t *testing.T) {
	reportRoot := os.Getenv("COMPSHARE_P3_RAW_REPORT_ROOT")
	if reportRoot == "" {
		t.Skip("set COMPSHARE_P3_RAW_REPORT_ROOT to verify the ignored production exports")
	}
	want := loadP3TraceReplayCases(t)
	got := collectP3TraceReplayCases(t, reportRoot)
	require.Equal(t, want, got, "verification compares only hashes and action families; raw content is never logged")
}

func assertHistoricalCatalogKeepsFullWindow(t *testing.T, category string) {
	t.Helper()
	scope := fullModelToolWindowScope(true, "historical_"+category)
	assert.Equal(t, tools.ToolScopeMutableFull, scope.Mode)
	assert.Equal(t, centralAgentToolNames(true, false), toolNames(centralAgentToolWindowForScope(true, false, scope)))
}

func loadP3TraceReplayCases(t *testing.T) []p3TraceReplayCase {
	t.Helper()
	path := filepath.Join("..", "..", "eval", "toolworkflow", "p3_trace_replay_manifest.jsonl")
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var cases []p3TraceReplayCase
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var c p3TraceReplayCase
		require.NoError(t, json.Unmarshal(line, &c))
		cases = append(cases, c)
	}
	require.NoError(t, scanner.Err())
	return cases
}

func collectP3TraceReplayCases(t *testing.T, reportRoot string) []p3TraceReplayCase {
	t.Helper()
	exports := []struct {
		name string
		path string
	}{
		{
			name: "20260626",
			path: filepath.Join(reportRoot, "prod_chat_export_20260626_183352", "raw", "agent_traces.jsonl"),
		},
		{
			name: "20260707_20260724",
			path: filepath.Join(reportRoot, "prod_chat_export_20260707_to_20260724_20260724_104943_raw_only", "raw", "agent_traces.jsonl"),
		},
	}
	counts := map[string]int{}
	var cases []p3TraceReplayCase
	for _, export := range exports {
		file, err := os.Open(export.path)
		require.NoError(t, err)
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			var row struct {
				ID          json.RawMessage `json:"id"`
				RequestUUID json.RawMessage `json:"request_uuid"`
				TurnIndex   int             `json:"turn_index"`
				TraceJSON   json.RawMessage `json:"trace_json"`
			}
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &row))
			actions := p3TraceActions(row.TraceJSON)
			category := p3ReplayCategory(actions, counts)
			if category == "" {
				continue
			}
			identity := fmt.Sprintf("%s|%s|%s|%d", export.name, p3RawIdentity(row.ID), p3RawIdentity(row.RequestUUID), row.TurnIndex)
			digest := sha256.Sum256([]byte(identity))
			cases = append(cases, p3TraceReplayCase{
				IDHash:   hex.EncodeToString(digest[:]),
				Export:   export.name,
				Category: category,
				Source:   "agent_trace",
			})
			counts[category]++
		}
		require.NoError(t, scanner.Err())
		require.NoError(t, file.Close())
	}
	return cases
}

func p3RawIdentity(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(bytes.TrimSpace(raw))
}

func p3TraceActions(raw json.RawMessage) map[string]struct{} {
	var trace struct {
		ToolCalls []struct {
			Action string `json:"action"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(raw, &trace) != nil {
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil {
			return nil
		}
		if json.Unmarshal([]byte(encoded), &trace) != nil {
			return nil
		}
	}
	actions := make(map[string]struct{}, len(trace.ToolCalls))
	for _, call := range trace.ToolCalls {
		if call.Action != "" {
			actions[call.Action] = struct{}{}
		}
	}
	return actions
}

func p3ReplayCategory(actions map[string]struct{}, counts map[string]int) string {
	for _, target := range p3ReplayCategoryTargets {
		if counts[target.category] >= target.limit {
			continue
		}
		for _, action := range target.actions {
			if _, ok := actions[action]; ok {
				return target.category
			}
		}
	}
	return ""
}
