package actionresolver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A UserRequestedOnlyFields field is accepted from the user and from nobody else.
// Every non-user source is refused with the SAME typed kind, so a caller can act
// on the reason without reading the message.
func TestUserRequestedOnlyFieldAcceptsOnlyTheUser(t *testing.T) {
	tests := []struct {
		source   CandidateSource
		accepted bool
	}{
		// Exactly one source passes: the user said it, this turn, in their own text.
		{source: SourceUserExplicit, accepted: true},
		{source: SourceAgentInference, accepted: false},
		{source: SourceToolObservation, accepted: false},
		// A server-derived binding is a strong source for a TARGET (it proves which
		// object the user meant). It proves nothing about whether the user wanted a
		// different operation performed on it.
		{source: SourceVerifiedContext, accepted: false},
		// Sounds like consent, is not reachable: its only producer is the sealed-
		// secret path, and a secret can never be a gated field. Refused rather than
		// left as an untakeable accept inside a consent gate.
		{source: SourceUserConfirmation, accepted: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.source), func(t *testing.T) {
			resolver := New(userRequestedOnlyCatalog(t), EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

			resolved := resolver.Resolve(ActionProposal{
				Operation: "GatedOp",
				Slots: []SlotCandidate{{
					Name: "Mode", Value: "A", Source: tt.source,
					Evidence: &SourceEvidence{Quote: "A"},
				}},
			})

			if tt.accepted {
				require.True(t, resolved.ReadyForConfirmation, resolved.Rejected)
				require.Equal(t, "A", resolved.Arguments["Mode"])
				return
			}
			require.False(t, resolved.ReadyForConfirmation)
			require.Contains(t, resolved.RejectedProblems,
				RejectedProblem{Slot: "Mode", Kind: RejectRequiresUserRequest})
			require.NotContains(t, resolved.Arguments, "Mode")
		})
	}
}

// The gate refuses the FIELD, not the operation: an ungated field of the same
// proposal still resolves, so the model is told precisely what to drop.
func TestTheGateRefusesTheFieldNotTheRestOfTheProposal(t *testing.T) {
	resolver := New(userRequestedOnlyCatalog(t), EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{
		Operation: "GatedOp",
		Slots: []SlotCandidate{
			{Name: "Mode", Value: "A", Source: SourceAgentInference},
			{Name: "Note", Value: "hello", Source: SourceAgentInference},
		},
	})

	require.Equal(t, "hello", resolved.Arguments["Note"])
	require.NotContains(t, resolved.Arguments, "Mode")
	require.Len(t, resolved.RejectedProblems, 1)
}

// An undeclared field of the same operation is untouched — the gate is a
// declaration, not a property of optional enums.
func TestAnUndeclaredEnumIsNotGated(t *testing.T) {
	resolver := New(userRequestedOnlyCatalog(t), EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{
		Operation: "GatedOp",
		Slots:     []SlotCandidate{{Name: "Other", Value: "X", Source: SourceAgentInference}},
	})

	require.True(t, resolved.ReadyForConfirmation, resolved.Rejected)
	require.Equal(t, "X", resolved.Arguments["Other"])
}

// A declaration that names nothing real would gate nothing while looking like it
// gated something — the failure mode a typo produces. Each guard fails the build
// of the catalog rather than the request.
func TestMarkUserRequestedOnlyRefusesADeclarationItCannotHonour(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{name: "unknown field", field: "Nope"},
		{name: "required field", field: "Must"},
		{name: "target field", field: "UHostId"},
		{name: "secret field", field: "Password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := map[string]FieldSpec{
				"Must":     {Name: "Must", Required: true, Codec: CodecEnum},
				"UHostId":  {Name: "UHostId", Target: true, TargetKind: "instance", Codec: CodecResourceRef},
				"Password": {Name: "Password", Codec: CodecSensitiveText},
			}

			require.Error(t, markUserRequestedOnly(fields, []string{tt.field}))
		})
	}
}

func TestMarkUserRequestedOnlyMarksExactlyWhatWasDeclared(t *testing.T) {
	fields := map[string]FieldSpec{
		"Mode":  {Name: "Mode", Codec: CodecEnum},
		"Other": {Name: "Other", Codec: CodecEnum},
	}

	require.NoError(t, markUserRequestedOnly(fields, []string{"Mode"}))
	require.True(t, fields["Mode"].RequiresUserRequest)
	require.False(t, fields["Other"].RequiresUserRequest)
}

func userRequestedOnlyCatalog(t *testing.T) *Catalog {
	t.Helper()
	fields := map[string]FieldSpec{
		"Mode":  {Name: "Mode", Codec: CodecEnum, Enum: []string{"A", "B"}},
		"Other": {Name: "Other", Codec: CodecEnum, Enum: []string{"X"}},
		"Note":  {Name: "Note", Codec: CodecConstrainedText},
	}
	require.NoError(t, markUserRequestedOnly(fields, []string{"Mode"}))
	return &Catalog{
		ordered: []string{"GatedOp"},
		specs:   map[string]OperationSpec{"GatedOp": {Operation: "GatedOp", Fields: fields}},
	}
}
