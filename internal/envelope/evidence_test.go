package envelope

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceFieldsAreUnexportedAndVisibilityTagged(t *testing.T) {
	typ := reflect.TypeOf(Evidence{})
	want := map[string]string{
		"sourceTitle":        "user",
		"snippet":            "user",
		"surfaceURL":         "user",
		"evidenceKind":       "user",
		"chunkID":            "trace",
		"kbVersion":          "trace",
		"retrievalScore":     "trace",
		"queryNormalized":    "trace",
		"apiRefs":            "trace",
		"toolCallID":         "trace",
		"producedAt":         "trace",
		"internalSourceID":   "internal",
		"approvalRecordHash": "internal",
		"originalCaseID":     "internal",
		"debugReason":        "internal",
	}
	require.Equal(t, len(want), typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		assert.NotEmpty(t, field.PkgPath, "%s must remain unexported", field.Name)
		assert.Equal(t, want[field.Name], field.Tag.Get("visibility"), field.Name)
		delete(want, field.Name)
	}
	assert.Empty(t, want)
}

func TestEvidenceProjectionFieldSetsAreStable(t *testing.T) {
	assert.Equal(t, []string{"SourceTitle", "Snippet", "SurfaceURL", "EvidenceKind"}, exportedFieldNames[UserView]())
	assert.Equal(t, []string{"SourceTitle", "EvidenceKind", "ChunkID", "KBVersion", "RetrievalScore", "QueryNormalized", "APIRefs", "ToolCallID", "ProducedAt", "SurfaceURL", "SurfaceURLRejectionReason"}, exportedFieldNames[TraceView]())
	assert.Equal(t, []string{"SourceTitle", "Snippet", "EvidenceKind"}, exportedFieldNames[LLMView]())
}

func TestEvidenceViewsDoNotExposeInternalOrTraceFieldsToWrongBoundary(t *testing.T) {
	surfaceURL := "https://console.compshare.cn/light-gpu/console/resources"
	internalSourceID := "feishu-doc-internal"
	approvalHash := "sha256:approval"
	originalCaseID := "case-123"
	debugReason := "raw internal context"
	score := 0.82
	e, err := NewEvidence(EvidenceInput{
		SourceTitle:        "Windows login",
		Snippet:            "Use the console RDP entry.",
		SurfaceURL:         &surfaceURL,
		EvidenceKind:       EvidenceKindKnowledge,
		ChunkID:            "w0-login-001",
		KBVersion:          "kb.stage2b.w0.2026-05-13",
		RetrievalScore:     &score,
		QueryNormalized:    "windows login",
		ProducedAt:         time.Unix(100, 0).UTC(),
		InternalSourceID:   &internalSourceID,
		ApprovalRecordHash: &approvalHash,
		OriginalCaseID:     &originalCaseID,
		DebugReason:        &debugReason,
	})
	require.NoError(t, err)

	user := e.ForUser()
	assert.Equal(t, "Windows login", user.SourceTitle)
	assert.Equal(t, "Use the console RDP entry.", user.Snippet)
	require.NotNil(t, user.SurfaceURL)
	assert.Equal(t, surfaceURL, *user.SurfaceURL)
	assertNoFields(t, user, "ChunkID", "KBVersion", "RetrievalScore", "InternalSourceID", "ApprovalRecordHash", "OriginalCaseID", "DebugReason")

	trace := e.ForTrace()
	assert.Equal(t, "Windows login", trace.SourceTitle)
	assert.Equal(t, "w0-login-001", trace.ChunkID)
	assert.Equal(t, "kb.stage2b.w0.2026-05-13", trace.KBVersion)
	assert.InDelta(t, 0.82, trace.RetrievalScore, 0.0001)
	assertNoFields(t, trace, "Snippet", "InternalSourceID", "ApprovalRecordHash", "OriginalCaseID", "DebugReason")

	llm := e.ForLLM()
	assert.Equal(t, "Windows login", llm.SourceTitle)
	assert.Equal(t, "Use the console RDP entry.", llm.Snippet)
	assertNoFields(t, llm, "ChunkID", "KBVersion", "RetrievalScore", "QueryNormalized", "SurfaceURL", "InternalSourceID", "ApprovalRecordHash", "OriginalCaseID", "DebugReason")
}

func TestEvidenceSurfaceURLPolicyInUserAndTraceViews(t *testing.T) {
	allowedConsole := "https://console.compshare.cn/instances"
	allowedDocs := "https://www.compshare.cn/docs/gpus/login"
	deniedGitLab := "https://gitlab.example.com/group/project"
	deniedAdmin := "https://console.compshare.cn/admin/users"
	deniedToken := "https://console.compshare.cn/instances?token=abc"

	for _, rawURL := range []string{allowedConsole, allowedDocs} {
		t.Run(rawURL, func(t *testing.T) {
			e := mustKnowledgeEvidence(t, rawURL)
			require.NotNil(t, e.ForUser().SurfaceURL)
			require.NotNil(t, e.ForTrace().SurfaceURL)
			assert.Nil(t, e.ForTrace().SurfaceURLRejectionReason)
		})
	}

	for _, tc := range []struct {
		rawURL string
		reason string
	}{
		{rawURL: deniedGitLab, reason: "denied_internal_host"},
		{rawURL: deniedAdmin, reason: "internal_path"},
		{rawURL: deniedToken, reason: "signed_url_query"},
	} {
		t.Run(tc.rawURL, func(t *testing.T) {
			e := mustKnowledgeEvidence(t, tc.rawURL)
			assert.Nil(t, e.ForUser().SurfaceURL)
			trace := e.ForTrace()
			assert.Nil(t, trace.SurfaceURL)
			require.NotNil(t, trace.SurfaceURLRejectionReason)
			assert.Equal(t, tc.reason, *trace.SurfaceURLRejectionReason)
		})
	}
}

func TestIsAllowedSurfaceURLPolicy(t *testing.T) {
	allowed := []string{
		"https://console.compshare.cn/instances",
		"https://console.compshare.cn/instances?e=event",
		"https://www.compshare.cn/docs/gpus/login",
	}
	for _, rawURL := range allowed {
		assert.True(t, IsAllowedSurfaceURL(rawURL).Allowed, rawURL)
	}

	rejected := map[string]string{
		"http://console.compshare.cn/instances":              "scheme_not_https",
		"https://gitlab.example.com/group/project":           "denied_internal_host",
		"https://foo.feishu.cn/docx/abc":                     "denied_internal_host",
		"https://www.lark.com/docx/abc":                      "denied_internal_host",
		"https://console.compshare.cn/workorder/1":           "internal_path",
		"https://console.compshare.cn/instances?signature=x": "signed_url_query",
		"https://download.compshare.cn/file?expires=1":       "temporary_download",
		"https://www.compshare.cn/pricing":                   "host_not_in_allowlist",
	}
	for rawURL, reason := range rejected {
		decision := IsAllowedSurfaceURL(rawURL)
		assert.False(t, decision.Allowed, rawURL)
		assert.Equal(t, reason, decision.Reason, rawURL)
	}
}

func TestNewEvidenceValidatesKindSpecificRequirements(t *testing.T) {
	score := 0.7
	_, err := NewEvidence(EvidenceInput{
		SourceTitle:  "missing chunk",
		Snippet:      "snippet",
		EvidenceKind: EvidenceKindKnowledge,
		KBVersion:    "kb",
		ProducedAt:   time.Unix(1, 0).UTC(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chunk_id")

	_, err = NewEvidence(EvidenceInput{
		SourceTitle:     "missing score",
		Snippet:         "snippet",
		EvidenceKind:    EvidenceKindKnowledge,
		ChunkID:         "chunk",
		KBVersion:       "kb",
		QueryNormalized: "query",
		ProducedAt:      time.Unix(1, 0).UTC(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retrieval_score")

	_, err = NewEvidence(EvidenceInput{
		SourceTitle:     "valid knowledge",
		Snippet:         "snippet",
		EvidenceKind:    EvidenceKindKnowledge,
		ChunkID:         "chunk",
		KBVersion:       "kb",
		RetrievalScore:  &score,
		QueryNormalized: "query",
		ProducedAt:      time.Unix(1, 0).UTC(),
	})
	require.NoError(t, err)

	_, err = NewEvidence(EvidenceInput{
		SourceTitle:  "api fact",
		Snippet:      "snippet",
		EvidenceKind: EvidenceKindAPIFact,
		APIRefs:      []string{"DescribeCompShareInstance"},
		ProducedAt:   time.Unix(1, 0).UTC(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_call_id")

	_, err = NewEvidence(EvidenceInput{
		SourceTitle:  "api fact",
		Snippet:      "snippet",
		EvidenceKind: EvidenceKindAPIFact,
		APIRefs:      []string{"DescribeCompShareInstance"},
		ToolCallID:   "tool-1",
		ProducedAt:   time.Unix(1, 0).UTC(),
	})
	require.NoError(t, err)
}

func TestNewEvidenceRequiresApprovalForOriginalCase(t *testing.T) {
	score := 0.7
	originalCaseID := "case-1"
	_, err := NewEvidence(EvidenceInput{
		SourceTitle:     "case rewrite",
		Snippet:         "snippet",
		EvidenceKind:    EvidenceKindKnowledge,
		ChunkID:         "chunk",
		KBVersion:       "kb",
		RetrievalScore:  &score,
		QueryNormalized: "query",
		ProducedAt:      time.Unix(1, 0).UTC(),
		OriginalCaseID:  &originalCaseID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approval_record_hash")
}

func TestKnowledgeNoEvidenceUsesOptionB(t *testing.T) {
	result := KnowledgeNoEvidence()

	assert.Equal(t, "我没有在知识库里找到可靠资料来回答这个问题。建议你在控制台对应模块查看，或联系平台客服确认。", result.Reply)
	assert.True(t, result.Trace.NoEvidence)
	assert.Equal(t, EvidenceKindKnowledge, result.Trace.EvidenceKind)
	assert.Empty(t, result.Trace.HitItems)
	assert.Equal(t, "retrieval_zero_hit", result.Trace.FallbackReason)
}

func exportedFieldNames[T any]() []string {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		out = append(out, typ.Field(i).Name)
	}
	return out
}

func assertNoFields(t *testing.T, value any, names ...string) {
	t.Helper()
	typ := reflect.TypeOf(value)
	for _, name := range names {
		_, ok := typ.FieldByName(name)
		assert.False(t, ok, "%T must not expose %s", value, name)
	}
}

func mustKnowledgeEvidence(t *testing.T, rawURL string) Evidence {
	t.Helper()
	score := 0.8
	e, err := NewEvidence(EvidenceInput{
		SourceTitle:     "title",
		Snippet:         "snippet",
		SurfaceURL:      &rawURL,
		EvidenceKind:    EvidenceKindKnowledge,
		ChunkID:         "chunk",
		KBVersion:       "kb",
		RetrievalScore:  &score,
		QueryNormalized: "query",
		ProducedAt:      time.Unix(1, 0).UTC(),
	})
	require.NoError(t, err)
	return e
}
