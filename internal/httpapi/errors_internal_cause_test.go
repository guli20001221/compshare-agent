package httpapi

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnclassifiedErrorIsNotEchoedToTheClient is the gate for a real leak.
//
// A stopped database produced this, verbatim, in the chat UI:
//
//	get session: dial tcp 127.0.0.1:15432: connectex: No connection could be
//	made because the target machine actively refused it.
//
// Two things are wrong with that. It names where the database lives — in
// production that is the real host and port, handed to whoever is chatting. And
// it is unactionable: the user cannot do anything with a driver message. The
// trigger is not exotic either (pool exhausted, failover, restart), so this is
// not a "only when something is already very broken" concern.
//
// The classified errors model what a user CAN be told; the default branch is by
// definition the failures nobody decided how to explain, so it must answer
// generically and leave the detail to the log.
func TestUnclassifiedErrorIsNotEchoedToTheClient(t *testing.T) {
	raw := fmt.Errorf("get session: %w",
		errors.New("dial tcp 10.0.0.7:5432: connectex: No connection could be made because the target machine actively refused it"))

	apiErr := AsAPIError(raw)
	require.NotNil(t, apiErr)

	assert.Equal(t, ErrInternal.Code, apiErr.Code)
	assert.Equal(t, ErrInternal.RetCode, apiErr.RetCode)
	assert.Equal(t, ErrInternal.Message, apiErr.Message,
		"an unclassified failure answers with the canned message, not with whatever the process failed on")
	assert.NotContains(t, apiErr.Message, "10.0.0.7",
		"the database host must never reach a client")
	assert.NotContains(t, apiErr.Message, "dial tcp")
	assert.NotContains(t, apiErr.Error(), "10.0.0.7",
		"Error() feeds Message, so it must not become a second way out")

	// The detail is kept, not discarded — this is what makes the swap safe.
	require.Error(t, apiErr.Cause())
	assert.Contains(t, apiErr.Cause().Error(), "10.0.0.7")
}

// TestConvertedErrorStaysInspectableByCallers: hiding the text from the USER must
// not hide the error from CODE. Callers branch on typed errors (sql.ErrNoRows is
// canonicalized to a 404 in writeError), and an APIError that swallowed its cause
// would silently turn every such branch into a 500.
func TestConvertedErrorStaysInspectableByCallers(t *testing.T) {
	apiErr := AsAPIError(fmt.Errorf("get session: %w", sql.ErrNoRows))
	assert.True(t, errors.Is(apiErr, sql.ErrNoRows),
		"errors.Is must see through the conversion, or typed handling downstream breaks")
}

// TestClassifiedErrorsPassThroughUnchanged pins the other half: this change is
// about the DEFAULT branch only. An error we did classify already carries a
// message written for a user, and it must reach them intact — including the ones
// that legitimately embed detail, like which Action was unsupported.
func TestClassifiedErrorsPassThroughUnchanged(t *testing.T) {
	explicit := ErrInvalidParam.WithMessage("unsupported Action %s", "CreateSession")
	got := AsAPIError(explicit)
	assert.Equal(t, "unsupported Action CreateSession", got.Message)
	assert.Equal(t, ErrInvalidParam.RetCode, got.RetCode)
	assert.Nil(t, got.Cause(), "a classified error has no hidden cause to log")

	wrapped := AsAPIError(fmt.Errorf("while leasing: %w", ErrSessionTurnLimit))
	assert.Equal(t, ErrSessionTurnLimit.Message, wrapped.Message,
		"a classified error wrapped by a caller is still that error")
	assert.Equal(t, ErrSessionTurnLimit.RetCode, wrapped.RetCode)
}

// TestErrInternalPrototypeIsNotMutated guards the copy in AsAPIError. Attaching
// the cause to the shared package-level ErrInternal instead of to a copy would
// leak one request's failure into every later one — including into responses for
// unrelated users.
func TestErrInternalPrototypeIsNotMutated(t *testing.T) {
	_ = AsAPIError(errors.New("first request: secret detail"))
	assert.Nil(t, ErrInternal.Cause(),
		"the shared prototype must never accumulate a request's cause")
	assert.Equal(t, "后端未预期错误", ErrInternal.Message)
}

func TestClassifyChatErrorDoesNotExposeProviderBody(t *testing.T) {
	const canary = `{"code":"server_error","message":"provider request req-case-131","help":"https://help.openai.com"}`
	raw := errors.New(canary)

	got := classifyChatError(raw)

	assert.Equal(t, ErrModelError.Code, got.Code)
	assert.Equal(t, ErrModelError.Message, got.Message)
	assert.NotContains(t, got.Message, "req-case-131")
	assert.NotContains(t, got.Message, "help.openai.com")
	assert.ErrorIs(t, got, raw, "the server-side typed cause remains available to traces/logs")
	assert.Nil(t, ErrModelError.Cause(), "the shared public prototype must remain immutable")
}
