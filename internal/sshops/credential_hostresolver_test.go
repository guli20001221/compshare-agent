package sshops

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// These cover the one decision the address rewrite makes: WHICH machine address the
// consented credential is pointed at. Getting it wrong is not a cosmetic bug — a lane
// that quietly keeps dialling an address the deployment cannot reach reports the
// resulting timeout as the instance's firewall, which is precisely the wrong-layer
// diagnosis #516 and #522 were spent removing.

type recordingResolver struct {
	host   string
	err    error
	calls  int
	sawIDs []string
}

func (r *recordingResolver) ResolveHost(_ context.Context, instance map[string]any) (string, error) {
	r.calls++
	id, _ := instance["UHostId"].(string)
	r.sawIDs = append(r.sawIDs, id)
	return r.host, r.err
}

func oneInstance(id, login string) *listDescriber {
	return &listDescriber{raw: map[string]any{"UHostSet": []any{instRow(id, login, pwPepper)}}}
}

// The wiring assertion: a resolver handed to NewService must reach the credential the
// harness is actually run with. Everything else here tests FetchCredential directly, so
// without this the option could be accepted, stored, and never consulted — a lane that
// looks configured and dials the old address anyway.
func TestServiceDialsTheResolvedAddress(t *testing.T) {
	run := &fakeRunner{res: Result{Output: "ok"}}
	svc := NewService(run, &MemAuditWriter{}, WithHostResolver(&recordingResolver{host: "2003:da8:2004:1000::9"}))
	d := oneInstance("uhost-wired", "ssh -p 23 root@10.0.0.7")

	_, err := svc.Diagnose(context.Background(), d, Owner{TurnID: "t1"}, "uhost-wired", "task", nil, nil)

	require.NoError(t, err)
	require.Equal(t, 1, run.calls)
	require.Equal(t, "2003:da8:2004:1000::9", run.lastCred.Host,
		"the harness must be handed the resolved address, not the advertised one")
}

func TestFetchCredentialWithoutResolverKeepsTheAdvertisedHost(t *testing.T) {
	d := oneInstance("uhost-plain", "ssh -p 23 root@10.0.0.7")

	cred, err := FetchCredentialWithHostResolver(context.Background(), d, "uhost-plain", nil)

	require.NoError(t, err)
	require.Equal(t, "10.0.0.7", cred.Host, "a nil resolver must leave the lane byte-identical to before")
	require.Equal(t, 23, cred.Port)
	require.Equal(t, "root", cred.User)
}

func TestFetchCredentialDialsTheResolvedAddressKeepingUserAndPort(t *testing.T) {
	// The user and the port must survive the rewrite. They are properties of the IMAGE,
	// not of the route: a container image answers on 23 as root over either address
	// (it runs with --net host, so its sshd is bound on the machine's own stack — the
	// same reason the public EIP's port 23 works today), and moving the host must not
	// quietly turn that into the host VM's own port-22 ubuntu account, which is a
	// different machine's worth of filesystem.
	r := &recordingResolver{host: "2003:da8:2004:1000::1"}
	d := oneInstance("uhost-container", "ssh -p 23 root@10.0.0.7")

	cred, err := FetchCredentialWithHostResolver(context.Background(), d, "uhost-container", r)

	require.NoError(t, err)
	require.Equal(t, "2003:da8:2004:1000::1", cred.Host)
	require.Equal(t, 23, cred.Port, "the port belongs to the image, not to the route")
	require.Equal(t, "root", cred.User, "the user belongs to the image, not to the route")
	require.True(t, cred.HasSecret())
	require.Equal(t, []string{"uhost-container"}, r.sawIDs,
		"the resolver must see the instance the credential was READ FROM, not a re-fetch")
}

func TestFetchCredentialRefusesWhenTheAddressRewriteFails(t *testing.T) {
	// The anti-fail-open assertion. A deployment only configures a resolver because the
	// advertised address does not work there; falling back to it on error would produce a
	// guaranteed timeout blamed on the customer's instance.
	boom := errors.New("gateway unreachable")
	r := &recordingResolver{err: boom}
	d := oneInstance("uhost-vm", "ssh ubuntu@10.0.0.9")

	cred, err := FetchCredentialWithHostResolver(context.Background(), d, "uhost-vm", r)

	require.ErrorIs(t, err, boom, "the underlying cause must survive so the log names the layer")
	require.Empty(t, cred.Host, "no dial target may survive a failed rewrite")
	require.False(t, cred.HasSecret(), "no password may survive a failed rewrite")
}

func TestFetchCredentialKeepsAdvertisedHostWhenResolverHasNoMapping(t *testing.T) {
	// ("", nil) is "nothing to rewrite here", which is a different answer from a failure
	// and must not be turned into one.
	r := &recordingResolver{host: ""}
	d := oneInstance("uhost-vm", "ssh ubuntu@10.0.0.9")

	cred, err := FetchCredentialWithHostResolver(context.Background(), d, "uhost-vm", r)

	require.NoError(t, err)
	require.Equal(t, "10.0.0.9", cred.Host)
	require.Equal(t, 1, r.calls)
}

func TestFetchCredentialNeverRewritesADNSLoginHost(t *testing.T) {
	// A Pod advertises `<podId>.podtcp.compshare.cn` with a CLUSTER-SIDE forwarded port.
	// That port lives on the forwarder, not on the box, so redirecting it at the box's own
	// address would dial a port nothing has ever served. The resolver must not even be
	// consulted — asking it would make the guard depend on it answering correctly.
	r := &recordingResolver{host: "2003:da8:2004:1000::1"}
	d := oneInstance("uhost-pod", "ssh -p 23973 root@cpod-abcdefg.podtcp.compshare.cn")

	cred, err := FetchCredentialWithHostResolver(context.Background(), d, "uhost-pod", r)

	require.NoError(t, err)
	require.Equal(t, "cpod-abcdefg.podtcp.compshare.cn", cred.Host)
	require.Equal(t, 23973, cred.Port)
	require.Zero(t, r.calls, "a non-IPv4 login host must not reach the resolver at all")
}

func TestFetchCredentialDoesNotRewriteAnAlreadyIPv6LoginHost(t *testing.T) {
	// Defense against double-application: if upstream ever starts advertising the internal
	// address itself, rewriting it again would be at best a no-op and at worst a lookup on
	// an address that has no IPv4 to map.
	r := &recordingResolver{host: "2003:da8:2004:1000::2"}
	d := oneInstance("uhost-v6", "ssh -p 23 root@2003:da8:2004:1000::5")

	cred, err := FetchCredentialWithHostResolver(context.Background(), d, "uhost-v6", r)

	require.NoError(t, err)
	require.Zero(t, r.calls, "an address that is already IPv6 needs no rewrite")
	// The login-command regex stops at the first colon, so the parsed host is a prefix of the
	// address rather than the whole of it. That is pre-existing and out of scope here; what
	// this pins is only that the rewrite does not fire.
	require.NotEqual(t, "2003:da8:2004:1000::2", cred.Host)
}
