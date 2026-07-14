package turncoord

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutionEnvelope_FreezesOnlyStableSecretFreeInputs(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 81, OrganizationID: 82}
	digest := StableImageDigest([]byte("same original image bytes"))
	model := "model-a"
	in := SubmitInput{
		Owner: owner, SessionID: "session", ClientTurnID: "client-turn",
		Message: "联系 private@example.com，目标 10.0.0.8 / project-live；secret_key=do-not-store", ImageContext: "CUDA OOM access_token=also-secret",
		ImageDigest: digest, AssistantModel: &model,
		UserContext: tools.UserContext{
			CompanyID: 7, AccountID: 8, Channel: 9,
			RoleUrn: "ucs:iam::81:role/operator", SessionName: "temporary-sts-session",
			ProjectId: "project-1", Region: "cn-bj2", UserEmail: "private@example.com",
			ClientIP: "203.0.113.9",
		},
	}
	envelope, raw, err := freezeSubmitInput(in)
	require.NoError(t, err)
	text := string(raw)
	assert.NotContains(t, text, "do-not-store")
	assert.NotContains(t, text, "also-secret")
	assert.Contains(t, text, "203.0.113.9")
	assert.NotContains(t, text, "temporary-sts-session")
	assert.Contains(t, text, "private@example.com")
	assert.Contains(t, text, "10.0.0.8")
	assert.Contains(t, text, "project-live")
	assert.Contains(t, strings.ToLower(text), "client_ip")
	assert.NotContains(t, strings.ToLower(text), "session_name")
	assert.Contains(t, text, digest)

	turn := store.Turn{
		ID: "turn", SessionID: in.SessionID, ClientTurnID: in.ClientTurnID,
		Owner: owner, ExecutionEnvelope: raw,
	}
	restored, err := thawSubmitInput(turn)
	require.NoError(t, err)
	assert.Equal(t, envelope.Message, restored.Message)
	assert.Equal(t, envelope.OCR, restored.ImageContext)
	assert.Equal(t, digest, restored.ImageDigest)
	assert.Equal(t, "81-82", restored.UserContext.SessionName)
	assert.Equal(t, "203.0.113.9", restored.UserContext.ClientIP)
	assert.Equal(t, "private@example.com", restored.UserContext.UserEmail)
}

func TestExecutionEnvelope_RequestIdentityUsesImageDigestNotOCR(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 91, OrganizationID: 92}
	digest := StableImageDigest([]byte("image-a"))
	base := SubmitInput{
		Owner: owner, SessionID: "session", ClientTurnID: "client-turn", Message: "这是什么？",
		ImageContext: "第一次 OCR", ImageDigest: digest,
	}
	a, _, err := freezeSubmitInput(base)
	require.NoError(t, err)
	base.ImageContext = "第二次 OCR 有少量漂移"
	b, _, err := freezeSubmitInput(base)
	require.NoError(t, err)
	assert.Equal(t,
		hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, a),
		hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, b),
		"the same image must keep one idempotent turn even if OCR drifts",
	)

	base.ImageDigest = StableImageDigest([]byte("image-b"))
	c, _, err := freezeSubmitInput(base)
	require.NoError(t, err)
	assert.NotEqual(t,
		hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, a),
		hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, c),
		"reusing a client turn id with a different image must conflict",
	)
}

func TestExecutionEnvelope_OCRWithoutImageDigestFailsClosed(t *testing.T) {
	_, _, err := freezeSubmitInput(SubmitInput{
		Owner:     store.Owner{TopOrganizationID: 1, OrganizationID: 2},
		SessionID: "session", ClientTurnID: "turn", Message: "question", ImageContext: "OCR",
	})
	require.ErrorIs(t, err, store.ErrInvalidArgument)
}
