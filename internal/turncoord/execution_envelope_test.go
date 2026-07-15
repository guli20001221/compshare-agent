package turncoord

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/guardrails"
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
		Message: "联系 private@example.com，目标 10.0.0.8 / project-live；secret_key=do-not-store", ImageContext: "CUDA OOM access_token=also-secret 密码为 Screen123!",
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
	assert.NotContains(t, text, "Screen123!")
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

func TestExecutionEnvelope_SealsPasswordAndRestoresItOnlyToTheWorkflowChannel(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 101, OrganizationID: 102}
	key := bytes.Repeat([]byte{0x5a}, 32)
	for _, tc := range []struct{ name, message string }{
		{name: "chinese", message: "重置 uhost-1 密码为 Aa123456!"},
		{name: "english", message: "reset uhost-1 pass" + "word: Aa123456!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope, raw, err := freezeSubmitInputWithSecretKey(SubmitInput{
				Owner: owner, SessionID: "session", ClientTurnID: "turn-" + tc.name, Message: tc.message,
			}, key)
			require.NoError(t, err)
			assert.NotContains(t, envelope.Message, "Aa123456!")
			assert.Contains(t, envelope.Message, guardrails.CredentialRedactedOutput)
			assert.NotContains(t, string(raw), "Aa123456!")
			assert.NotEmpty(t, envelope.SealedSecrets)

			restored, err := thawSubmitInputWithSecretKey(store.Turn{
				Owner: owner, SessionID: "session", ClientTurnID: "turn-" + tc.name, ExecutionEnvelope: raw,
			}, key)
			require.NoError(t, err)
			assert.Equal(t, "Aa123456!", restored.SecretInputs["Password"])
			assert.NotContains(t, restored.Message, "Aa123456!")
		})
	}
}

func TestExecutionEnvelope_PasswordRequiresTheClusterSecretKey(t *testing.T) {
	_, _, err := freezeSubmitInput(SubmitInput{
		Owner:     store.Owner{TopOrganizationID: 1, OrganizationID: 2},
		SessionID: "session", ClientTurnID: "turn", Message: "密码为 Aa123456!",
	})
	require.ErrorIs(t, err, store.ErrInvalidArgument)
}

func TestExecutionEnvelope_PasswordQuestionsRemainIntactAndDoNotRequireASecretKey(t *testing.T) {
	for _, question := range []string{"密码怎么改？", "重置密码需要停机吗", "密码是怎么改的？", "密码是不是需要定期修改？"} {
		t.Run(question, func(t *testing.T) {
			envelope, raw, err := freezeSubmitInput(SubmitInput{
				Owner:     store.Owner{TopOrganizationID: 1, OrganizationID: 2},
				SessionID: "session", ClientTurnID: question, Message: question,
			})
			require.NoError(t, err)
			assert.Equal(t, question, envelope.Message)
			assert.Contains(t, string(raw), question)
			assert.Empty(t, envelope.SealedSecrets)
		})
	}
}

func TestExecutionEnvelope_ShiAssignmentIsSealedButQuestionsRemainPlain(t *testing.T) {
	key := bytes.Repeat([]byte{0x71}, 32)
	message := "重置密码是 Aa12" + "3456!"
	envelope, raw, err := freezeSubmitInputWithSecretKey(SubmitInput{
		Owner:     store.Owner{TopOrganizationID: 1, OrganizationID: 2},
		SessionID: "session", ClientTurnID: "shi-assignment", Message: message,
	}, key)
	require.NoError(t, err)
	assert.NotContains(t, envelope.Message, "Aa123456!")
	assert.NotContains(t, string(raw), "Aa123456!")
	assert.NotEmpty(t, envelope.SealedSecrets)
	_, _, err = freezeSubmitInput(SubmitInput{
		Owner:     store.Owner{TopOrganizationID: 1, OrganizationID: 2},
		SessionID: "session", ClientTurnID: "shi-without-key", Message: message,
	})
	require.ErrorIs(t, err, store.ErrInvalidArgument)
}

func TestExecutionEnvelope_V1IsRejectedInsteadOfSendingLegacyPlaintextToTheModel(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	_, err := thawSubmitInputWithSecretKey(store.Turn{
		Owner: owner, SessionID: "session", ClientTurnID: "legacy",
		ExecutionEnvelope: json.RawMessage(`{"version":1,"message":"密码为 LegacyPass123!","user_context":{"top_organization_id":1,"organization_id":2},"features":{}}`),
	}, bytes.Repeat([]byte{0x44}, 32))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported execution envelope version 1")
}

func TestExecutionEnvelope_SecretIdentityIsStableButDetectsChangedPassword(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 111, OrganizationID: 112}
	key := bytes.Repeat([]byte{0x35}, 32)
	base := SubmitInput{Owner: owner, SessionID: "session", ClientTurnID: "turn", Message: "密码为 Aa123456!"}
	a, _, err := freezeSubmitInputWithSecretKey(base, key)
	require.NoError(t, err)
	b, _, err := freezeSubmitInputWithSecretKey(base, key)
	require.NoError(t, err)
	assert.NotEqual(t, a.SealedSecrets, b.SealedSecrets, "encryption must use a fresh nonce")
	assert.Equal(t, hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, a),
		hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, b))

	base.Message = "密码为 Bb123456!"
	c, _, err := freezeSubmitInputWithSecretKey(base, key)
	require.NoError(t, err)
	assert.NotEqual(t, hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, a),
		hashExecutionEnvelope(owner, base.SessionID, base.ClientTurnID, c))
}

func TestExecutionEnvelope_SealedPasswordCannotBeOpenedWithAnotherKey(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 121, OrganizationID: 122}
	key := bytes.Repeat([]byte{0x11}, 32)
	_, raw, err := freezeSubmitInputWithSecretKey(SubmitInput{
		Owner: owner, SessionID: "session", ClientTurnID: "turn", Message: "密码为 Aa123456!",
	}, key)
	require.NoError(t, err)
	_, err = thawSubmitInputWithSecretKey(store.Turn{
		Owner: owner, SessionID: "session", ClientTurnID: "turn", ExecutionEnvelope: raw,
	}, bytes.Repeat([]byte{0x22}, 32))
	require.ErrorIs(t, err, store.ErrInvalidArgument)
}
