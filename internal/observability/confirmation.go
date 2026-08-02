package observability

import "strings"

// NormalizeConfirmationTerminalReason keeps confirmation attribution bounded
// even when a legacy boolean callback cannot report why it returned false.
func NormalizeConfirmationTerminalReason(confirmed bool, reason string) string {
	if confirmed {
		return ConfirmationReasonUserConfirmed
	}
	switch strings.TrimSpace(reason) {
	case ConfirmationReasonUserDeclined,
		ConfirmationReasonTimeout,
		ConfirmationReasonClientDisconnect,
		ConfirmationReasonDeliveryFailed,
		ConfirmationReasonBrokerCancelled:
		return strings.TrimSpace(reason)
	default:
		return ConfirmationReasonUserDeclined
	}
}

func ConfirmationStateForTerminalReason(reason string) string {
	if strings.TrimSpace(reason) == ConfirmationReasonUserConfirmed {
		return ConfirmationStateConfirmed
	}
	return ConfirmationStateNotConfirmed
}

func ConfirmationReasonIsUserCancellation(reason string) bool {
	switch strings.TrimSpace(reason) {
	case ConfirmationReasonUserDeclined, ConfirmationReasonTimeout, ConfirmationReasonClientDisconnect:
		return true
	default:
		return false
	}
}

func ConfirmationReasonIsError(reason string) bool {
	switch strings.TrimSpace(reason) {
	case ConfirmationReasonDeliveryFailed, ConfirmationReasonBrokerCancelled:
		return true
	default:
		return false
	}
}
