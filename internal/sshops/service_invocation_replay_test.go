package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/stretchr/testify/require"
)

// Enforce the same tuple as the real audit UNIQUE constraint. MemAuditWriter
// alone records duplicate Begins and cannot prove a replay was stopped.
type invocationUniqueAudit struct {
	MemAuditWriter
	seen map[string]bool
}

func (a *invocationUniqueAudit) Begin(ctx context.Context, event AuditEvent) (string, error) {
	key := event.TurnID + "\x00" + event.TaskHash
	if a.seen == nil {
		a.seen = make(map[string]bool)
	}
	if a.seen[key] {
		return "", fmt.Errorf("duplicate turn/invocation")
	}
	a.seen[key] = true
	return a.MemAuditWriter.Begin(ctx, event)
}

func TestServiceInvocationReplayCannotRunAfterArgumentOrContextChanges(t *testing.T) {
	d := stubDescriber{resp: describeResp("ssh -p 23 root@10.0.0.9", base64.StdEncoding.EncodeToString([]byte(secretPW)))}
	runner := &fakeRunner{res: Result{Output: "observed"}}
	audit := &invocationUniqueAudit{}
	svc := NewService(runner, audit)
	owner := Owner{TurnID: "turn-one", InvocationID: "call-one"}
	_, err := svc.DiagnoseWithContext(context.Background(), d, owner, "uhost-abc", "inspect service", opscontext.Context{}, nil, nil)
	require.NoError(t, err)
	_, err = svc.DiagnoseWithContext(context.Background(), d, owner, "uhost-abc", "restart service", opscontext.Context{
		SchemaVersion:       opscontext.SchemaVersion,
		ConversationHistory: []opscontext.ConversationMessage{{Role: "user", Content: "different context"}},
	}, nil, nil)
	require.ErrorContains(t, err, "audit begin failed")
	require.Equal(t, 1, runner.calls)
	owner.InvocationID = "call-two"
	_, err = svc.Diagnose(context.Background(), d, owner, "uhost-abc", "inspect service", nil, nil)
	require.NoError(t, err, "an intentional later invocation is not delivery replay")
	require.Equal(t, 2, runner.calls)
	owner.TurnID = "turn-two"
	owner.InvocationID = "call-one"
	_, err = svc.Diagnose(context.Background(), d, owner, "uhost-abc", "inspect service", nil, nil)
	require.NoError(t, err, "provider call IDs may recur in a different user turn")
	require.Equal(t, 3, runner.calls)
	require.Len(t, audit.Events, 6)
	require.NotEqual(t, audit.Events[0].TaskHash, audit.Events[2].TaskHash)
	require.Equal(t, audit.Events[0].TaskHash, audit.Events[4].TaskHash)
	require.Equal(t, "inspect service", audit.Events[0].Task)
}
