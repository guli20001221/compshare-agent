package turncoord

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func acceptFrozenTurn(t *testing.T, turns *store.PostgresTurnStore, in SubmitInput) store.Turn {
	t.Helper()
	envelope, raw, err := freezeSubmitInput(in)
	require.NoError(t, err)
	turn, _, err := turns.AcceptTurn(context.Background(), in.Owner, store.AcceptTurnInput{
		SessionID: in.SessionID, ClientTurnID: in.ClientTurnID,
		RequestHash: hashExecutionEnvelope(in.Owner, in.SessionID, in.ClientTurnID, envelope),
		UserContent: envelope.Message, AssistantModel: envelope.AssistantModel, ExecutionEnvelope: raw,
	})
	require.NoError(t, err)
	return turn
}

func TestCoordinator_StartupRecoversEveryOrphanExecutionStateFromEnvelope(t *testing.T) {
	for _, status := range []store.TurnStatus{
		store.TurnStatusAccepted,
		store.TurnStatusRunning,
		store.TurnStatusAwaitingConfirmation,
		store.TurnStatusCommitting,
	} {
		t.Run(string(status), func(t *testing.T) {
			db := openActionJournalTestDB(t)
			ctx := context.Background()
			owner := store.Owner{TopOrganizationID: 82101, OrganizationID: uint32(82110 + len(status))}
			sessions := store.NewSessionStore(db)
			session, err := sessions.Create(ctx, owner, nil, nil)
			require.NoError(t, err)
			turns := store.NewPostgresTurnStore(db)
			turn := acceptFrozenTurn(t, turns, SubmitInput{
				Owner: owner, SessionID: session.ID, ClientTurnID: "orphan-" + string(status), Message: "resume from database",
				UserContext: tools.UserContext{UserEmail: "recover@example.com", ClientIP: "198.51.100.18"},
			})
			if status != store.TurnStatusAccepted {
				lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "dead-replica", time.Minute)
				require.NoError(t, err)
				_, err = db.Exec(`UPDATE chat_turns SET status = $1 WHERE id = $2`, status, turn.ID)
				require.NoError(t, err)
				_, err = db.Exec(`UPDATE conversation_leases SET lease_until = NOW() - INTERVAL '1 second' WHERE session_id = $1`, lease.SessionID)
				require.NoError(t, err)
			}

			seenUser := make(chan tools.UserContext, 1)
			factory := &coordinatorFactory{newChat: func(int) func(context.Context, func(engine.StepEvent), engine.ChatOptions) (string, error) {
				return func(callCtx context.Context, _ func(engine.StepEvent), _ engine.ChatOptions) (string, error) {
					user, _ := tools.UserFrom(callCtx)
					seenUser <- user
					return "answer", nil
				}
			}}
			c := NewCoordinator(turns, sessions, EngineFactoryFunc(factory.New), Options{
				ReplicaID: "recovery-replica", LeaseTTL: 500 * time.Millisecond,
				LeaseRenewInterval: 100 * time.Millisecond, InteractionPoll: 10 * time.Millisecond,
				RecoveryScanInterval: 20 * time.Millisecond, ExecutionTimeout: 3 * time.Second,
			})
			t.Cleanup(c.Close)
			committed := waitTurnStatus(t, turns, owner, turn.ID, store.TurnStatusCommitted)
			assert.Equal(t, store.TurnStatusCommitted, committed.Status)
			assert.Empty(t, committed.ExecutionEnvelope, "terminal commit must erase the short-lived recovery envelope")
			select {
			case user := <-seenUser:
				assert.Equal(t, "recover@example.com", user.UserEmail)
				assert.Equal(t, "198.51.100.18", user.ClientIP)
			case <-time.After(2 * time.Second):
				t.Fatal("recovered engine did not receive the frozen upstream identity")
			}
			factory.mu.Lock()
			assert.Equal(t, 1, factory.calls)
			factory.mu.Unlock()
		})
	}
}

func TestCoordinator_SubscribeRecoversTurnAcceptedAfterStartupScan(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx := context.Background()
	owner := store.Owner{TopOrganizationID: 82201, OrganizationID: 82202}
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := store.NewPostgresTurnStore(db)
	factory := &coordinatorFactory{}
	c := NewCoordinator(turns, sessions, EngineFactoryFunc(factory.New), Options{
		ReplicaID: "resume-replica", LeaseTTL: time.Second, LeaseRenewInterval: 100 * time.Millisecond,
		InteractionPoll: 10 * time.Millisecond, RecoveryScanInterval: time.Hour, ExecutionTimeout: 3 * time.Second,
	})
	t.Cleanup(c.Close)
	time.Sleep(50 * time.Millisecond) // let the empty startup scan finish

	turn := acceptFrozenTurn(t, turns, SubmitInput{
		Owner: owner, SessionID: session.ID, ClientTurnID: "resume-after-startup", Message: "resume now",
	})
	received := make(chan Event, 8)
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, c.Subscribe(subCtx, owner, turn.ID, 0, func(event Event) error {
		received <- event
		return nil
	}))
	waitTurnStatus(t, turns, owner, turn.ID, store.TurnStatusCommitted)
	foundCommitted := false
	deadline := time.After(2 * time.Second)
	for !foundCommitted {
		select {
		case event := <-received:
			foundCommitted = event.Type == "turn.committed"
		case <-deadline:
			t.Fatal("resume subscriber did not receive the terminal event")
		}
	}
}
