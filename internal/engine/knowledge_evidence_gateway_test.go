package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type streamingEvidenceGatewayLLM struct{ mockLLM }

func (m *streamingEvidenceGatewayLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	resp, err := m.mockLLM.Chat(ctx, req)
	if err == nil && resp != nil && req.OnTextDelta != nil && len(resp.ToolCalls) == 0 {
		req.OnTextDelta(resp.Content)
	}
	return resp, err
}

func TestEvidenceGatewayMissingPlatformFactRetrievesOnceAndReplacesDraft(t *testing.T) {
	main := &streamingEvidenceGatewayLLM{mockLLM: mockLLM{responses: []llm.ChatResponse{
		{Content: "实例关机后永远不会被回收。"},
		{Content: `{"answer_question":"按量实例关机后多久会回收","search_queries":["按量实例关机后多久会被回收？"]}`},
		{Content: "按量实例连续关机达到规定期限后会被平台回收，重要数据应提前备份。"},
	}}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"knowledge_missing"}`},
		{Content: `{"decision":"pass","reason":"supported"}`},
	}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, HybridMode: knowledge.RetrievalModeQwen3RRF,
		HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 0.01, Chunk: knowledge.KBChunk{
			ChunkID: "chunk-lifecycle", Title: "实例回收规则", SourceType: "platform",
			ProductArea: "uhost", SourceOrigin: "platform_docs", Confidence: "high",
			Content: "按量实例连续关机达到规定期限后会被回收，系统盘数据将清除。",
		}}},
	}}}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(retriever)

	var streamed strings.Builder
	reply, err := eng.ChatWithOptions(context.Background(), "按量实例关机后多久会被回收？", noopStep, ChatOptions{
		OnTextDelta: func(delta string) { streamed.WriteString(delta) },
	})
	require.NoError(t, err)
	assert.Equal(t, "按量实例连续关机达到规定期限后会被平台回收，重要数据应提前备份。", reply)
	assert.Equal(t, reply, streamed.String())
	assert.NotContains(t, reply, "永远不会")
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, 1, eng.evidenceCorrectionCountThisTurn)
	assert.Equal(t, evidenceDecisionPass, eng.evidenceDecisionThisTurn)
	assert.True(t, eng.evidenceRequiredThisTurn)
	assert.True(t, eng.evidenceHadThisTurn)

	// The host-owned evidence id remains in the tool transcript, but the semantic
	// checker sees only numbered facts and is never asked to echo a chunk id.
	require.Len(t, gateway.calls, 2)
	assert.NotContains(t, gateway.calls[1].Messages[1].Content, "chunk-lifecycle")
	assert.Contains(t, gateway.calls[1].Messages[1].Content, `"product_area":"uhost"`)
	assert.Contains(t, gateway.calls[1].Messages[1].Content, `"source_origin":"platform_docs"`)
	assert.Contains(t, gateway.calls[1].Messages[1].Content, `"confidence":"high"`)
	assert.NotContains(t, gateway.calls[1].Messages[1].Content, `"strength"`)
	assert.NotContains(t, gateway.calls[1].Messages[1].Content, `"below_floor"`,
		"a kept raw RRF score is not a comparable confidence scale")
	for _, message := range eng.MessagesSnapshot() {
		assert.NotEqual(t, "实例关机后永远不会被回收。", message.Content,
			"the rejected draft must not enter canonical history")
	}
}

func TestEvidenceGatewayGeneralAnswerPassesWithoutRetrievalOrRewrite(t *testing.T) {
	main := &mockLLM{responses: []llm.ChatResponse{{Content: "可以先用 curl -I 检查 HTTP 响应头。"}}}
	gateway := &mockLLM{responses: []llm.ChatResponse{{Content: `{"decision":"pass","reason":"general"}`}}}
	retriever := &scriptedKnowledgeRetriever{}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "怎么检查一个网址是否可访问？", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "可以先用 curl -I 检查 HTTP 响应头。", reply)
	assert.Empty(t, retriever.calls)
	assert.Zero(t, eng.evidenceCorrectionCountThisTurn)
	assert.False(t, eng.evidenceRequiredThisTurn)
	assert.Equal(t, evidenceDecisionPass, eng.evidenceDecisionThisTurn)
}

func TestEvidenceGatewayCannotPassSupportedWhenEvidenceWasOmitted(t *testing.T) {
	gateway := &mockLLM{responses: []llm.ChatResponse{{Content: `{"decision":"pass","reason":"supported"}`}}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	for i := 0; i < maxEvidenceGatewayFacts+1; i++ {
		eng.searchKnowledgeLedgerThisTurn.Items = append(eng.searchKnowledgeLedgerThisTurn.Items, knowledge.EvidenceItem{
			ChunkID: "chunk-" + string(rune('a'+i)), Snippet: "platform fact",
		})
	}

	verdict, failure := eng.assessKnowledgeEvidence(context.Background(), "平台规则是什么？", "规则如下。")
	require.Empty(t, failure)
	assert.Equal(t, evidenceGatewayVerdict{Decision: evidenceDecisionRetrieve, Reason: evidenceReasonEvidenceInsufficient}, verdict)
	require.Len(t, gateway.calls, 1)
	assert.Contains(t, gateway.calls[0].Messages[1].Content, `"evidence_omitted":1`)
}

func TestEvidenceGatewayCannotPassSupportedWithoutEvidence(t *testing.T) {
	gateway := &mockLLM{responses: []llm.ChatResponse{{Content: `{"decision":"pass","reason":"supported"}`}}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)

	verdict, failure := eng.assessKnowledgeEvidence(context.Background(), "优云的工单入口在哪？", "右上角有工单入口。")
	require.Empty(t, failure)
	assert.Equal(t, evidenceGatewayVerdict{Decision: evidenceDecisionRetrieve, Reason: evidenceReasonKnowledgeMissing}, verdict)
}

func TestEvidenceGatewayExistingEvidenceMisuseGetsOneRewriteWithoutDuplicateSearch(t *testing.T) {
	main := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("search", "SearchKnowledge", `{"query":"云存储 Pro 上传文件"}`)}},
		{Content: `{"answer_question":"云存储 Pro 如何上传文件","search_queries":["云存储 Pro 上传文件"]}`},
		{Content: "可以在普通对象存储页面直接上传到云存储 Pro。"},
		{Content: "云存储 Pro 需要先挂载到实例，再通过挂载目录读写文件。"},
	}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"evidence_insufficient"}`},
		{Content: `{"decision":"pass","reason":"supported"}`},
	}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true,
		HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
			ChunkID: "chunk-storage-pro", Title: "云存储 Pro", SourceType: "platform",
			Content: "云存储 Pro 需要挂载到实例，通过挂载目录进行文件读写。",
		}}},
	}}}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "云存储 Pro 怎么上传文件？", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "云存储 Pro 需要先挂载到实例，再通过挂载目录读写文件。", reply)
	require.Len(t, retriever.calls, 1, "existing evidence should be rewritten, not blindly searched twice")
	assert.Equal(t, 1, eng.evidenceCorrectionCountThisTurn)
	require.GreaterOrEqual(t, len(main.calls), 4)
	last := main.calls[len(main.calls)-1]
	foundInstruction := false
	for _, message := range last.Messages {
		if strings.Contains(message.Content, "上一版候选答复没有展示给用户") {
			foundInstruction = true
		}
	}
	assert.True(t, foundInstruction)
}

func TestEvidenceGatewayReadsAFullChunkBeforeCorrectingInsufficientEvidence(t *testing.T) {
	main := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("search", "SearchKnowledge", `{"query":"镜像删除文件后为什么不变小"}`)}},
		{Content: `{"answer_question":"镜像删除文件后为什么不变小","search_queries":["镜像删除文件后为什么不变小"]}`},
		{Content: "把原镜像部署到小系统盘再制作就会缩小。"},
		{Content: "删除旧镜像层中的文件只会写入删除标记；要显著缩小，应从干净的小基础镜像重建。"},
	}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"evidence_insufficient"}`},
		{Content: `{"decision":"pass","reason":"supported"}`},
	}}
	full := knowledge.KBChunk{
		ChunkID: "chunk-image-layers", Title: "镜像分层", SourceType: "platform",
		Content: "删除已有层中的文件只会新增删除标记，不会回收原层。要显著缩小，应从较小、干净的基础镜像重新创建。",
	}
	retriever := &remoteChunkStoreRetriever{
		scriptedKnowledgeRetriever: scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
			Enabled: true, SearchID: "search-1", HybridMode: knowledge.RetrievalModeQwen3RRF,
			HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 0.9, Chunk: knowledge.KBChunk{
				ChunkID: full.ChunkID, Title: full.Title, SourceType: full.SourceType,
				Content: "镜像制作说明。后文包含分层与缩容限制。",
			}}},
		}}},
		chunks: map[string]knowledge.KBChunk{full.ChunkID: full},
	}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "删除镜像里的大文件为什么没变小？", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "干净的小基础镜像")
	require.Len(t, retriever.reads, 1)
	assert.Equal(t, []string{full.ChunkID}, retriever.reads[0].chunkIDs)
	assert.Equal(t, 1, eng.evidenceCorrectionCountThisTurn)
}

func TestEvidenceGatewaySecondUnsupportedDraftAbstainsAndStops(t *testing.T) {
	main := &streamingEvidenceGatewayLLM{mockLLM: mockLLM{responses: []llm.ChatResponse{
		{Content: "控制台里肯定有工单中心。"},
		{Content: `{"answer_question":"优云工单入口在哪里","search_queries":["优云工单入口"]}`},
		{Content: "请进入控制台右上角工单中心。"},
	}}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"knowledge_missing"}`},
		{Content: `{"decision":"retrieve","reason":"knowledge_missing"}`},
	}}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}})

	var streamed strings.Builder
	reply, err := eng.ChatWithOptions(context.Background(), "从哪里提交工单？", noopStep, ChatOptions{
		OnTextDelta: func(delta string) { streamed.WriteString(delta) },
	})
	require.NoError(t, err)
	assert.Contains(t, reply, "证据不足")
	assert.NotContains(t, reply, "工单中心")
	assert.Equal(t, reply, streamed.String())
	assert.Equal(t, evidenceDecisionAbstain, eng.evidenceDecisionThisTurn)
	assert.Equal(t, evidenceOutcomeCorrectionExhausted, eng.evidenceReasonThisTurn)
	assert.Equal(t, 1, eng.evidenceCorrectionCountThisTurn)
	assert.Len(t, main.calls, 3, "the gateway permits at most one correction")
	for _, message := range eng.MessagesSnapshot() {
		assert.NotContains(t, message.Content, "控制台里肯定有工单中心")
		assert.NotContains(t, message.Content, "请进入控制台右上角工单中心")
	}
	snapshot := eng.TraceSnapshot(time.Now())
	assert.Equal(t, evidenceDecisionAbstain, snapshot.EvidenceDecision)
	assert.Equal(t, evidenceOutcomeCorrectionExhausted, snapshot.EvidenceReason)
	assert.Equal(t, 1, snapshot.EvidenceCorrectionCount)
}

func TestEvidenceGatewayFailureDoesNotPublishUncheckedDraft(t *testing.T) {
	main := &mockLLM{responses: []llm.ChatResponse{{Content: "原答案"}}}
	gateway := &mockLLM{responses: []llm.ChatResponse{{Content: `{"decision":"maybe"}`}}}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)

	reply, err := eng.Chat(context.Background(), "解释一下", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "无法完成证据校验")
	assert.NotContains(t, reply, "原答案")
	assert.Equal(t, evidenceDecisionUnavailable, eng.evidenceDecisionThisTurn)
	assert.Equal(t, evidenceOutcomeGatewayUnavailable, eng.evidenceReasonThisTurn)
}

func TestEvidenceGatewayChecksNarrationBeforePrependingSensitiveReply(t *testing.T) {
	const token = "server-only-jupyter-token"
	main := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read-token", capability.ReadToolName(intent.IntentInstanceAccess),
			`{"targets":[{"type":"uhost_id_user_input","value":"uhost-1","source":"user_text"}],"access_type":"jupyter_token"}`)}},
		{Content: "Token 已获取，请到控制台右上角工单中心继续。"},
		{Content: "Token 已安全获取；现有证据无法确认优云存在工单入口。"},
	}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"evidence_insufficient"}`},
		{Content: `{"decision":"pass","reason":"supported"}`},
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "vm-a", "State": "Running", "InstanceType": "UHost",
			}},
		},
		"DescribeCompShareJupyterToken": {"JupyterToken": token},
	}}
	eng := NewWithDeps(main, executor, nil)
	eng.SetEvidenceGatewayClient(gateway)

	reply, err := eng.Chat(context.Background(), "查询 uhost-1 的 Jupyter Token", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, token)
	assert.NotContains(t, reply, "工单中心继续")
	assert.Len(t, gateway.calls, 2, "the server-owned secret must not exempt the model-authored narration")
	for _, call := range gateway.calls {
		for _, message := range call.Messages {
			assert.NotContains(t, message.Content, token, "the secret stays outside both model calls")
		}
	}
}

func TestEvidenceGatewayUnknownToolCannotBuyBypass(t *testing.T) {
	main := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("invented", "InventedPlatformTool", `{}`)}},
		{Content: "控制台一定有隐藏的工单入口。"},
		{Content: `{"answer_question":"优云工单入口","search_queries":["优云工单入口"]}`},
		{Content: "现有资料无法确认优云存在工单入口，我不猜测具体路径。"},
	}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"knowledge_missing"}`},
		{Content: `{"decision":"pass","reason":"general"}`},
	}}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}})

	reply, err := eng.Chat(context.Background(), "优云工单入口在哪里？", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "无法确认")
	assert.NotContains(t, reply, "隐藏的工单入口")
	assert.Len(t, gateway.calls, 2, "an unadvertised tool must not disable final evidence validation")
	assert.Equal(t, evidenceDecisionPass, eng.evidenceDecisionThisTurn)
}

func TestEvidenceGatewayFailedAdvertisedToolCannotBuyBypass(t *testing.T) {
	main := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("bad-request", "RequestResetPassword", `not-json`)}},
		{Content: "控制台一定有隐藏的工单入口。"},
		{Content: `{"answer_question":"优云工单入口","search_queries":["优云工单入口"]}`},
		{Content: "现有资料无法确认优云存在工单入口，我不猜测具体路径。"},
	}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"knowledge_missing"}`},
		{Content: `{"decision":"pass","reason":"general"}`},
	}}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}})

	reply, err := eng.Chat(context.Background(), "优云工单入口在哪里？", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "无法确认")
	assert.NotContains(t, reply, "隐藏的工单入口")
	assert.Len(t, gateway.calls, 2,
		"an advertised tool that returned needs_input must not disable final evidence validation")
}

func TestEvidenceGatewayUsesAuthorizationFreeQuestionForAssessmentAndRetrieval(t *testing.T) {
	const (
		secret          = "gateway-secret-must-not-leak-0123456789"
		signedURLSecret = "signed-url-secret-must-not-leak-0123456789"
	)
	main := &mockLLM{responses: []llm.ChatResponse{
		{Content: "请到工单中心提交。"},
		{Content: `{"answer_question":"优云工单入口","search_queries":["优云工单入口"]}`},
		{Content: "现有资料无法确认优云存在工单入口。"},
	}}
	gateway := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"decision":"retrieve","reason":"knowledge_missing"}`},
		{Content: `{"decision":"pass","reason":"general"}`},
	}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}}
	eng := NewWithDeps(main, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "Authorization: Bearer "+secret+
		"\nhttps://example.test/file?Authorization="+signedURLSecret+"\n优云工单入口在哪里？", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "无法确认")
	for _, call := range append(append([]llm.ChatRequest(nil), main.calls...), gateway.calls...) {
		for _, message := range call.Messages {
			assert.NotContains(t, message.Content, secret)
			for _, toolCall := range message.ToolCalls {
				assert.NotContains(t, toolCall.Function.Arguments, signedURLSecret,
					"a host-forced retrieval call must not copy a signed URL credential")
			}
		}
	}
	for _, call := range gateway.calls {
		for _, message := range call.Messages {
			assert.NotContains(t, message.Content, signedURLSecret)
		}
	}
	require.NotEmpty(t, retriever.calls)
	for _, call := range retriever.calls {
		assert.NotContains(t, call.question, secret)
		assert.NotContains(t, call.question, signedURLSecret)
	}
	for _, message := range eng.MessagesSnapshot() {
		assert.NotContains(t, message.Content, secret)
		for _, toolCall := range message.ToolCalls {
			assert.NotContains(t, toolCall.Function.Arguments, signedURLSecret)
		}
	}
}
