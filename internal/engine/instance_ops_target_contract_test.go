package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/sshops"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

const targetLogFollowup = "ssh -vvv -p 29222 root@192.0.2.1\nOpenSSH_for_Windows_9.5p1, LibreSSL 3.8.2\ndebug1: Connecting to 192.0.2.1 [192.0.2.1] port 29222.\ndebug2: fd 3 setting O_NONBLOCK\ndebug1: connect to address 192.0.2.1 port 29222: Connection timed out\nssh: connect to host 192.0.2.1 port 29222: Connection timed out"

func targetContractState() SessionState {
	return SessionState{
		SchemaVersion: SessionStateSchemaCurrent, SelectedInstanceID: "uhost-picked",
		SelectedInstanceName: "host", SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: time.Now().Add(-time.Minute).Unix(), SelectedInstanceFreshness: ContinuityFreshnessFresh,
	}
}

func syncTargetContractRegistry(t *testing.T, eng *Engine, firstName, secondName string) {
	t.Helper()
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2), "UHostSet": []any{
			map[string]any{"UHostId": "uhost-picked", "Name": firstName, "State": "Running"},
			map[string]any{"UHostId": "uhost-next", "Name": secondName, "State": "Running"},
		},
	}, "test"))
}

func TestOrdinaryTurnEntryNeverReinterpretsTargetText(t *testing.T) {
	for _, question := range []string{targetLogFollowup, "host", "新训练机 上的服务起不来", "uhost-next", "uhost-picked 和 uhost-next", "第2台"} {
		t.Run(question, func(t *testing.T) {
			model := &mockLLM{responses: []llm.ChatResponse{{Content: "已收到。"}}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)
			state := targetContractState()
			eng.SetSessionState(state, 1)
			syncTargetContractRegistry(t, eng, "host", "host")
			_, err := eng.Chat(context.Background(), question, noopStep)
			require.NoError(t, err)
			got, _, _ := eng.SessionStateSnapshot()
			require.Equal(t, state, got, "a name/ID/ordinal in ordinary dialogue must not preempt the central Agent's interpretation")
		})
	}
}

// These are transport/authority contracts, not claims about mock model quality.
// The central model selects the target; names, history and OCR do not run a
// second selection algorithm between its tool call and the scoped runner.
func TestInstanceOpsPassesModelTargetWithoutNaturalLanguageRebinding(t *testing.T) {
	for _, tc := range []struct {
		name, firstName, secondName, question, target string
		initiallyUnselected                           bool
	}{
		{"historical055 duplicate host in real log shape", "host", "host", targetLogFollowup, "uhost-picked", false},
		{"other instance uniquely named host in log", "alpha", "host", targetLogFollowup, "uhost-picked", false},
		{"duplicate names do not replace exact model ID", "host", "host", "host", "uhost-next", false},
		{"explicit switch reaches the new ID", "host", "host", "现在排查 uhost-next", "uhost-next", false},
		{"model resolves a reference from conversation", "alpha", "beta", "继续", "uhost-next", false},
		{"model selection does not require stored selection proof", "alpha", "beta", "请排查 beta 的服务", "uhost-next", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "已检查", Ran: 1}}
			model := &mockLLM{responses: []llm.ChatResponse{{ToolCalls: []openai.ToolCall{
				toolCall("diagnose", "DiagnoseInstanceInternals", `{"UHostId":"`+tc.target+`","Task":"检查服务"}`),
			}}}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)
			eng.SetInstanceOps(runner)
			initialState := targetContractState()
			if tc.initiallyUnselected {
				initialState = SessionState{SchemaVersion: SessionStateSchemaCurrent}
			}
			eng.SetSessionState(initialState, 1)
			syncTargetContractRegistry(t, eng, tc.firstName, tc.secondName)
			_, err := eng.Chat(context.Background(), tc.question, noopStep)
			require.NoError(t, err)
			require.Equal(t, 1, runner.calls)
			require.Equal(t, tc.target, runner.lastReq.InstanceID)
			state, _, _ := eng.SessionStateSnapshot()
			require.Equal(t, tc.target, state.SelectedInstanceID)
			if tc.target != "uhost-picked" {
				require.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource,
					"model choice must not mint a user_selected proof for platform writes")
			}
		})
	}
}

// Keep the production credential resolver inside the test: returning the wrong
// row must fail its exact-ID check, not a replacement mock authorization check.
type targetCredentialDescriber struct {
	rowID, encoded string
	err            error
	user           tools.UserContext
	action         string
	args           map[string]any
}

func (d *targetCredentialDescriber) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	d.user, _ = tools.UserFrom(ctx)
	d.action, d.args = action, args
	if d.err != nil {
		return nil, d.err
	}
	return map[string]any{"UHostSet": []any{map[string]any{
		"UHostId": d.rowID, "State": "Running", "SshLoginCommand": "ssh -p 23 root@192.0.2.1", "Password": d.encoded,
	}}}, nil
}

type credentialCheckedTargetRunner struct {
	describer *targetCredentialDescriber
	enteredID string
}

func (r *credentialCheckedTargetRunner) Run(ctx context.Context, req InstanceOpsRequest, progress func(InstanceOpsProgress)) (InstanceOpsVerdict, error) {
	cred, err := sshops.FetchCredential(ctx, r.describer, req.InstanceID)
	if errors.Is(err, sshops.ErrInstanceNotFound) {
		return InstanceOpsVerdict{}, ErrInstanceOpsNotFound
	}
	if err != nil {
		return InstanceOpsVerdict{}, err
	}
	r.enteredID = cred.InstanceID
	progress(InstanceOpsProgress{Kind: InstanceOpsProgressConnected})
	return InstanceOpsVerdict{Text: "已检查", Ran: 1}, nil
}

func TestModelSelectedTargetRetainsTenantAndExactCredentialBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, rowID, encoded string
		err                  error
		entered              bool
	}{
		{name: "exact account row", rowID: "uhost-next", encoded: "Zml4dHVyZQ==", entered: true},
		{name: "requested target not in account", rowID: "uhost-other", encoded: "Zml4dHVyZQ=="},
		{name: "tenant credential lookup denied", err: errors.New("tenant-scoped STS lookup denied")},
		{name: "credential unavailable", rowID: "uhost-next"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &targetCredentialDescriber{rowID: tc.rowID, encoded: tc.encoded, err: tc.err}
			runner := &credentialCheckedTargetRunner{describer: d}
			model := &mockLLM{responses: []llm.ChatResponse{{ToolCalls: []openai.ToolCall{
				toolCall("diagnose", "DiagnoseInstanceInternals", `{"UHostId":"uhost-next","Task":"检查服务"}`),
			}}}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)
			eng.SetInstanceOps(runner)
			eng.SetSessionState(targetContractState(), 1)
			user := tools.UserContext{TopOrganizationID: 123, OrganizationID: 456, ProjectId: "test-project"}
			reply, err := eng.Chat(tools.WithUser(context.Background(), user), "继续", noopStep)
			require.NoError(t, err)
			require.Equal(t, user, d.user, "request tenant identity must reach credential lookup unchanged")
			require.Equal(t, "DescribeCompShareInstance", d.action)
			require.Equal(t, map[string]any{"UHostIds.0": "uhost-next"}, d.args)
			state, _, _ := eng.SessionStateSnapshot()
			if tc.entered {
				require.Equal(t, "uhost-next", runner.enteredID)
				require.Equal(t, "uhost-next", state.SelectedInstanceID)
				require.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource)
			} else {
				require.Empty(t, runner.enteredID, "no credential means no SSH entry")
				require.Equal(t, "uhost-picked", state.SelectedInstanceID, "a failed proposal must not become verified target context")
				require.NotContains(t, reply, "已检查")
			}
		})
	}
}
