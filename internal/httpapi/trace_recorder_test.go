package httpapi

import (
	"errors"
	"testing"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatTraceRecorderMarksChatError(t *testing.T) {
	writer := &captureTraceWriter{}
	recorder := newChatTraceRecorder(
		writer,
		BaseRequest{
			RequestUUID: "req-error",
			Owner: store.Owner{
				TopOrganizationID: 1,
				OrganizationID:    2,
			},
		},
		"sess-error",
		1,
		"hi",
		time.Now(),
	)

	err := recorder.Finish(errors.New("boom"), time.Now())
	require.NoError(t, err)
	require.Len(t, writer.records, 1)
	assert.Equal(t, observability.EngineHardBlockTrace{
		Hit:      true,
		Category: "chat_error",
	}, writer.records[0].EngineHardBlock)
}
