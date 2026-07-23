package workflow

import (
	"errors"
	"strings"
)

// MissingSlotError is the transport-neutral workflow signal that required
// structured arguments are absent. It contains field identities only; parsing
// user prose and composing clarifications belong to the central Agent.
type MissingSlotError struct {
	Message string
	Slots   []string
}

func (e MissingSlotError) Error() string { return e.Message }

func NewMissingSlotError(message string, slots ...string) error {
	return MissingSlotError{Message: strings.TrimSpace(message), Slots: uniqueSlotNames(slots)}
}

func MissingSlotsFromError(err error) []string {
	var missing MissingSlotError
	if errors.As(err, &missing) {
		return uniqueSlotNames(missing.Slots)
	}
	return nil
}

func uniqueSlotNames(slots []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(slots))
	for _, slot := range slots {
		slot = strings.TrimSpace(slot)
		if slot == "" {
			continue
		}
		if _, exists := seen[slot]; exists {
			continue
		}
		seen[slot] = struct{}{}
		out = append(out, slot)
	}
	return out
}
