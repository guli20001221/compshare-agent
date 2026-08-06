package sshops

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These cover the one question that decides which machine a user's consent
// actually authorizes: when the describe response does not contain the instance
// that was asked for, does the fetch refuse, or does it hand back some other
// box's credential under the requested id?
//
// The request filters by UHostIds.0, so in the happy case the response holds
// exactly the requested instance and none of this fires. It fires when that
// filter is not honored — which is why the fixtures below return a list the
// caller did not ask for. A live probe of the filter would not remove the need
// for this: the failure is silent, it is inside one tenant (so credential
// scoping cannot catch it), and it corrupts the consent card and the audit row
// in the same step.

type listDescriber struct {
	raw    map[string]any
	gotAct string
	gotArg map[string]any
}

func (d *listDescriber) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	d.gotAct, d.gotArg = action, args
	return d.raw, nil
}

func instRow(id, login, b64pw string) map[string]any {
	return map[string]any{"UHostId": id, "SshLoginCommand": login, "Password": b64pw}
}

// cGVwcGVy = base64("pepper"), c2FsdA== = base64("salt")
const (
	pwPepper = "cGVwcGVy"
	pwSalt   = "c2FsdA=="
)

func TestFetchCredentialRefusesWhenRequestedInstanceAbsent(t *testing.T) {
	d := &listDescriber{raw: map[string]any{"UHostSet": []any{
		instRow("uhost-other-1", "ssh -p 23 root@10.0.0.1", pwPepper),
		instRow("uhost-other-2", "ssh ubuntu@10.0.0.2", pwSalt),
	}}}

	cred, err := FetchCredential(context.Background(), d, "uhost-requested")

	require.Error(t, err, "a response without the requested instance must not yield a credential")
	require.Contains(t, err.Error(), "uhost-requested")
	require.False(t, cred.HasSecret(), "no password may survive a failed resolve")
	require.Empty(t, cred.Host, "no connection target may survive a failed resolve")
	// The caller has to be able to tell "this id is not in the account" (retrying is pointless)
	// from a transient describe failure (retrying is the right advice). Without the sentinel both
	// reached the user as 「请稍后重试」, and on an account whose ids go stale within the hour that
	// is the single most common way this lane fails.
	require.ErrorIs(t, err, ErrInstanceNotFound,
		"an absent instance must be distinguishable from a transient failure")
}

// An EMPTY response is deliberately NOT ErrInstanceNotFound: nothing came back at all, which a
// partial upstream failure also looks like, and there retrying genuinely is the right advice.
// This is the boundary that keeps the new sentinel honest rather than a catch-all.
func TestFetchCredentialEmptyResponseIsNotTreatedAsNotFound(t *testing.T) {
	d := &listDescriber{raw: map[string]any{"UHostSet": []any{}}}

	_, err := FetchCredential(context.Background(), d, "uhost-x")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInstanceNotFound,
		"an empty response may be a transient upstream failure, not proof the instance is gone")
}

func TestFetchCredentialResolvesTheRequestedRowNotTheFirst(t *testing.T) {
	d := &listDescriber{raw: map[string]any{"UHostSet": []any{
		instRow("uhost-first", "ssh -p 23 root@10.0.0.1", pwPepper),
		instRow("uhost-wanted", "ssh -p 24962 root@abc.podtcp.compshare.cn", pwSalt),
		instRow("uhost-third", "ssh ubuntu@10.0.0.3", pwPepper),
	}}}

	cred, err := FetchCredential(context.Background(), d, "uhost-wanted")

	require.NoError(t, err)
	require.Equal(t, "uhost-wanted", cred.InstanceID)
	require.Equal(t, "abc.podtcp.compshare.cn", cred.Host, "took the connection details off the wrong row")
	require.Equal(t, 24962, cred.Port)
	require.Equal(t, "root", cred.User)
}

// The id on the returned credential is what the caller shows on the consent card
// and writes to the audit row, so it has to come from the row the connection
// details came from — not from the argument.
func TestFetchCredentialReportsTheResolvedIDNotTheRequestedOne(t *testing.T) {
	d := &listDescriber{raw: map[string]any{"UHostSet": []any{
		instRow("uhost-wanted", "ssh ubuntu@10.0.0.9", pwPepper),
	}}}

	cred, err := FetchCredential(context.Background(), d, "uhost-wanted")

	require.NoError(t, err)
	require.Equal(t, "uhost-wanted", cred.InstanceID)
	require.Equal(t, "10.0.0.9", cred.Host)

	// Same fixture, different ask: the row is present but is not the one asked
	// for, so there is nothing to return.
	_, err = FetchCredential(context.Background(), d, "uhost-somebody-else")
	require.Error(t, err)
}

func TestFetchCredentialEmptyResponseAndFilterShape(t *testing.T) {
	d := &listDescriber{raw: map[string]any{"UHostSet": []any{}}}

	_, err := FetchCredential(context.Background(), d, "uhost-x")

	require.Error(t, err)
	require.Contains(t, err.Error(), "no instance")
	require.Equal(t, "DescribeCompShareInstance", d.gotAct)
	require.Equal(t, map[string]any{"UHostIds.0": "uhost-x"}, d.gotArg,
		"the id must be sent as an upstream filter, not resolved client-side only")
}

// A row that matches by id but cannot be connected to must fail rather than fall
// through to a row that can.
func TestFetchCredentialDoesNotSubstituteAConnectableRow(t *testing.T) {
	d := &listDescriber{raw: map[string]any{"UHostSet": []any{
		instRow("uhost-wanted", "", pwPepper), // Windows / no SSH
		instRow("uhost-other", "ssh ubuntu@10.0.0.4", pwSalt),
	}}}

	_, err := FetchCredential(context.Background(), d, "uhost-wanted")

	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "SshLoginCommand"),
		"expected the no-SSH-target error for the matched row, got: %v", err)
}
