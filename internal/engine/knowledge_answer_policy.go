package engine

import "github.com/compshare-agent/internal/knowledge"

// Knowledge-answer validation is typography-only and fail-open. It records weak
// evidence and verbatim echoes but never replaces the Agent's answer with a
// canned refusal.
const (
	weakEvidenceBM25Threshold     = knowledge.WeakEvidenceBM25Threshold
	weakEvidenceSemanticThreshold = knowledge.WeakEvidenceSemanticThreshold
)
