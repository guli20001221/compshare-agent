package envelope

import (
	"errors"
	"fmt"
	"time"
)

type EvidenceKind string

const (
	EvidenceKindKnowledge      EvidenceKind = "knowledge"
	EvidenceKindAPIFact        EvidenceKind = "api_fact"
	EvidenceKindDiagnosis      EvidenceKind = "diagnosis"
	EvidenceKindWorkflowResult EvidenceKind = "workflow_result"
)

const KnowledgeNoEvidenceReply = "我没有在知识库里找到可靠资料来回答这个问题。建议你在控制台对应模块查看，或联系平台客服确认。"

type Evidence struct {
	sourceTitle        string       `visibility:"user"`
	snippet            string       `visibility:"user"`
	surfaceURL         *string      `visibility:"user"`
	evidenceKind       EvidenceKind `visibility:"user"`
	chunkID            string       `visibility:"trace"`
	kbVersion          string       `visibility:"trace"`
	retrievalScore     float64      `visibility:"trace"`
	queryNormalized    string       `visibility:"trace"`
	apiRefs            []string     `visibility:"trace"`
	toolCallID         string       `visibility:"trace"`
	producedAt         time.Time    `visibility:"trace"`
	internalSourceID   *string      `visibility:"internal"`
	approvalRecordHash *string      `visibility:"internal"`
	originalCaseID     *string      `visibility:"internal"`
	debugReason        *string      `visibility:"internal"`
}

type EvidenceInput struct {
	SourceTitle        string
	Snippet            string
	SurfaceURL         *string
	EvidenceKind       EvidenceKind
	ChunkID            string
	KBVersion          string
	RetrievalScore     *float64
	QueryNormalized    string
	APIRefs            []string
	ToolCallID         string
	ProducedAt         time.Time
	InternalSourceID   *string
	ApprovalRecordHash *string
	OriginalCaseID     *string
	DebugReason        *string
}

type UserView struct {
	SourceTitle  string       `json:"source_title"`
	Snippet      string       `json:"snippet"`
	SurfaceURL   *string      `json:"surface_url"`
	EvidenceKind EvidenceKind `json:"evidence_kind"`
}

type TraceView struct {
	SourceTitle               string       `json:"source_title"`
	EvidenceKind              EvidenceKind `json:"evidence_kind"`
	ChunkID                   string       `json:"chunk_id,omitempty"`
	KBVersion                 string       `json:"kb_version,omitempty"`
	RetrievalScore            float64      `json:"retrieval_score,omitempty"`
	QueryNormalized           string       `json:"query_normalized,omitempty"`
	APIRefs                   []string     `json:"api_refs,omitempty"`
	ToolCallID                string       `json:"tool_call_id,omitempty"`
	ProducedAt                time.Time    `json:"produced_at"`
	SurfaceURL                *string      `json:"surface_url,omitempty"`
	SurfaceURLRejectionReason *string      `json:"surface_url_rejection_reason,omitempty"`
}

type LLMView struct {
	SourceTitle  string       `json:"source_title"`
	Snippet      string       `json:"snippet"`
	EvidenceKind EvidenceKind `json:"evidence_kind"`
}

type TraceHitItem struct {
	ChunkID string  `json:"chunk_id"`
	Score   float64 `json:"score"`
	Kept    bool    `json:"kept"`
}

type NoEvidenceTraceView struct {
	EvidenceKind   EvidenceKind   `json:"evidence_kind"`
	HitItems       []TraceHitItem `json:"hit_items"`
	NoEvidence     bool           `json:"no_evidence"`
	FallbackReason string         `json:"fallback_reason"`
}

type NoEvidenceResult struct {
	Reply string              `json:"reply"`
	Trace NoEvidenceTraceView `json:"trace"`
}

func NewEvidence(input EvidenceInput) (Evidence, error) {
	if input.SourceTitle == "" {
		return Evidence{}, errors.New("source_title is required")
	}
	if input.Snippet == "" {
		return Evidence{}, errors.New("snippet is required")
	}
	if input.ProducedAt.IsZero() {
		return Evidence{}, errors.New("produced_at is required")
	}
	if input.OriginalCaseID != nil && input.ApprovalRecordHash == nil {
		return Evidence{}, errors.New("approval_record_hash required when original_case_id is set")
	}

	var retrievalScore float64
	switch input.EvidenceKind {
	case EvidenceKindKnowledge:
		if input.ChunkID == "" {
			return Evidence{}, errors.New("chunk_id is required for knowledge evidence")
		}
		if input.KBVersion == "" {
			return Evidence{}, errors.New("kb_version is required for knowledge evidence")
		}
		if input.RetrievalScore == nil {
			return Evidence{}, errors.New("retrieval_score is required for knowledge evidence")
		}
		if input.QueryNormalized == "" {
			return Evidence{}, errors.New("query_normalized is required for knowledge evidence")
		}
		retrievalScore = *input.RetrievalScore
	case EvidenceKindAPIFact:
		if len(input.APIRefs) == 0 {
			return Evidence{}, errors.New("api_refs is required for api_fact evidence")
		}
		if input.ToolCallID == "" {
			return Evidence{}, errors.New("tool_call_id is required for api_fact evidence")
		}
	case EvidenceKindDiagnosis, EvidenceKindWorkflowResult:
		if input.ToolCallID == "" {
			return Evidence{}, fmt.Errorf("tool_call_id is required for %s evidence", input.EvidenceKind)
		}
	case "":
		return Evidence{}, errors.New("evidence_kind is required")
	default:
		return Evidence{}, fmt.Errorf("unsupported evidence_kind %q", input.EvidenceKind)
	}

	return Evidence{
		sourceTitle:        input.SourceTitle,
		snippet:            input.Snippet,
		surfaceURL:         cloneStringPtr(input.SurfaceURL),
		evidenceKind:       input.EvidenceKind,
		chunkID:            input.ChunkID,
		kbVersion:          input.KBVersion,
		retrievalScore:     retrievalScore,
		queryNormalized:    input.QueryNormalized,
		apiRefs:            append([]string(nil), input.APIRefs...),
		toolCallID:         input.ToolCallID,
		producedAt:         input.ProducedAt,
		internalSourceID:   cloneStringPtr(input.InternalSourceID),
		approvalRecordHash: cloneStringPtr(input.ApprovalRecordHash),
		originalCaseID:     cloneStringPtr(input.OriginalCaseID),
		debugReason:        cloneStringPtr(input.DebugReason),
	}, nil
}

func (e Evidence) ForUser() UserView {
	return UserView{
		SourceTitle:  e.sourceTitle,
		Snippet:      e.snippet,
		SurfaceURL:   allowedSurfaceURL(e.surfaceURL).url,
		EvidenceKind: e.evidenceKind,
	}
}

func (e Evidence) ForTrace() TraceView {
	surface := allowedSurfaceURL(e.surfaceURL)
	return TraceView{
		SourceTitle:               e.sourceTitle,
		EvidenceKind:              e.evidenceKind,
		ChunkID:                   e.chunkID,
		KBVersion:                 e.kbVersion,
		RetrievalScore:            e.retrievalScore,
		QueryNormalized:           e.queryNormalized,
		APIRefs:                   append([]string(nil), e.apiRefs...),
		ToolCallID:                e.toolCallID,
		ProducedAt:                e.producedAt,
		SurfaceURL:                surface.url,
		SurfaceURLRejectionReason: surface.rejectionReason,
	}
}

func (e Evidence) ForLLM() LLMView {
	return LLMView{
		SourceTitle:  e.sourceTitle,
		Snippet:      e.snippet,
		EvidenceKind: e.evidenceKind,
	}
}

func KnowledgeNoEvidence() NoEvidenceResult {
	return NoEvidenceResult{
		Reply: KnowledgeNoEvidenceReply,
		Trace: NoEvidenceTraceView{
			EvidenceKind:   EvidenceKindKnowledge,
			HitItems:       []TraceHitItem{},
			NoEvidence:     true,
			FallbackReason: "retrieval_zero_hit",
		},
	}
}

type projectedSurfaceURL struct {
	url             *string
	rejectionReason *string
}

func allowedSurfaceURL(raw *string) projectedSurfaceURL {
	if raw == nil {
		return projectedSurfaceURL{}
	}
	decision := IsAllowedSurfaceURL(*raw)
	if decision.Allowed {
		return projectedSurfaceURL{url: cloneStringPtr(raw)}
	}
	return projectedSurfaceURL{rejectionReason: &decision.Reason}
}

func cloneStringPtr(in *string) *string {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
