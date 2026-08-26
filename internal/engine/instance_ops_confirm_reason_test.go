package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/require"
)

// harnessDispositionForTerminalReason mirrors the compatibility translation in harness.py. New
// product runs approve the whole repair scope at entry, but a mixed-version deployment or a direct
// fail-closed caller can still produce these terminal reasons until both sides have rolled forward.
func harnessDispositionForTerminalReason(reason string) string {
	switch reason {
	case observability.ConfirmationReasonUserDeclined:
		return "refused_user_declined"
	case observability.ConfirmationReasonTimeout:
		return "refused_confirmation_timeout"
	case observability.ConfirmationReasonClientDisconnect:
		return "refused_client_disconnect"
	case observability.ConfirmationReasonDeliveryFailed:
		return "refused_confirmation_delivery_failed"
	case observability.ConfirmationReasonBrokerCancelled:
		return "refused_confirmation_broker_cancelled"
	default:
		return "refused_not_approved"
	}
}

func TestHarnessAndEngineAgreeOnEveryConfirmationDisposition(t *testing.T) {
	src, err := os.ReadFile("../../deploy/ssh_ops_harness/harness.py")
	require.NoError(t, err, "the harness is part of the same deploy; a moved file must fail here")

	block := regexp.MustCompile(`(?s)_CONFIRMATION_REFUSAL_DISPOSITIONS\s*=\s*\{(.*?)\}`).FindSubmatch(src)
	require.Len(t, block, 2,
		"could not find _CONFIRMATION_REFUSAL_DISPOSITIONS in harness.py — this test must fail loudly "+
			"rather than silently verify an empty set")
	pairs := regexp.MustCompile(`"([a-z_]+)"\s*:\s*"([a-z_]+)"`).FindAllStringSubmatch(string(block[1]), -1)
	require.NotEmpty(t, pairs, "the mapping block parsed to zero entries")

	fallback := instanceOpsRefusalReason("refused_something_added_later")
	declined := instanceOpsRefusalReason("refused_user_declined")
	for _, p := range pairs {
		terminalReason, disposition := p[1], p[2]
		require.Equal(t, disposition, harnessDispositionForTerminalReason(terminalReason),
			"harness.py maps %q -> %q; the mirror in this package disagrees", terminalReason, disposition)
		got := instanceOpsRefusalReason(disposition)
		require.NotEqual(t, fallback, got,
			"%q reaches the engine with no sentence of its own and degrades to the collapsed one", disposition)
		if disposition != "refused_user_declined" {
			require.NotEqual(t, declined, got,
				"%q renders identically to an explicit decline", disposition)
		}
	}

	inTable := map[string]bool{}
	for _, p := range pairs {
		inTable[p[1]] = true
	}
	for _, reason := range []string{
		observability.ConfirmationReasonUserDeclined,
		observability.ConfirmationReasonTimeout,
		observability.ConfirmationReasonClientDisconnect,
		observability.ConfirmationReasonDeliveryFailed,
		observability.ConfirmationReasonBrokerCancelled,
	} {
		require.True(t, inTable[reason],
			"the transport can end a compatibility card with %q and harness.py has no entry for it", reason)
	}
}

// Structured tools also emit refused_precondition when a hash is stale, a match is ambiguous or a
// job id cannot be resolved. Adding a future wire refusal without user-facing wording must fail.
func TestHarnessAndEngineAgreeOnEveryRefusalDisposition(t *testing.T) {
	src, err := os.ReadFile("../../deploy/ssh_ops_harness/harness.py")
	require.NoError(t, err, "the harness is part of the same deploy; a moved file must fail here")

	block := regexp.MustCompile(`(?s)_DISPOSITION_MAP\s*=\s*\{(.*?)\}`).FindSubmatch(src)
	require.Len(t, block, 2,
		"could not find _DISPOSITION_MAP in harness.py — this test must fail loudly rather than verify an empty set")
	pairs := regexp.MustCompile(`"([a-z_]+)"\s*:\s*"([a-z_]+)"`).FindAllStringSubmatch(string(block[1]), -1)
	require.NotEmpty(t, pairs, "the disposition map parsed to zero entries")
	fallback := instanceOpsRefusalReason("refused_something_added_later")
	seenRefusal := false
	for _, pair := range pairs {
		if pair[2] != "refused" {
			continue
		}
		seenRefusal = true
		require.True(t, strings.HasPrefix(pair[1], "refused_"),
			"a wire refusal should carry a refusal-shaped reason, got %q", pair[1])
		require.NotEqual(t, fallback, instanceOpsRefusalReason(pair[1]),
			"%q is emitted as refused but has no actionable engine wording", pair[1])
	}
	require.True(t, seenRefusal, "the wire map contains no refused dispositions")
}
