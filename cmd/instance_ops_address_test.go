package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/sshops"
	"github.com/stretchr/testify/require"
)

// The sshops sentinel has to survive the adapter boundary, or the engine's honest message is
// unreachable and every failed rewrite reads as "please retry" — advice that cannot work when
// the cause is the deployment's own gateway. The wrapping is real (%w over the underlying
// error), so this also pins that errors.Is still matches through it.
func TestInstanceOpsRunner_TranslatesAddressUnavailableSentinel(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	wrapped := fmt.Errorf("%w: %w", sshops.ErrInternalAddressUnavailable,
		errors.New("uvpc http: dial tcp 10.0.0.1:80: i/o timeout"))
	r := newInstanceOpsRunner(&fakeDiagnoser{err: wrapped}, noopDescriber{}, nil)

	_, err := r.Run(context.Background(), engine.InstanceOpsRequest{InstanceID: "uhost-x", TurnID: "t1"}, func(engine.InstanceOpsProgress) {})

	require.ErrorIs(t, err, engine.ErrInstanceOpsAddressUnavailable,
		"the engine mirror must be returned so the reply can name the layer")
	// And the operator still gets the verbatim cause: which of the gateway, the region lookup or
	// the transform failed is not in the user-facing sentence, and should not be.
	require.Contains(t, buf.String(), "i/o timeout")
	require.Contains(t, buf.String(), "uhost-x")
}
