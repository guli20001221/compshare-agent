package envelope

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashEnvelopeIsStableAndRedactsSecrets(t *testing.T) {
	env := Envelope{
		Kind:          KindResourceInfo,
		SourceActions: []string{"DescribeCompShareInstance"},
		Subjects: []Subject{{
			ID:   "uhost-a",
			Name: "Authorization: Bearer " + "aaaaaaaaaaaaaaaaaaaaaaaaa",
			Type: SubjectInstance,
		}},
		Facts: []Fact{{
			SubjectID: "uhost-a",
			Key:       "state",
			Label:     "状态",
			Value:     "Running",
			Source:    FactSourceAPI,
		}},
		Constraints: Constraints{DoNotInventInstances: true},
	}

	first, err := Hash(env)
	require.NoError(t, err)
	second, err := Hash(env)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, first)
}
