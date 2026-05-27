package security

import (
	"testing"
)

func TestCheck_L0_DirectExecute(t *testing.T) {
	l0Actions := []string{
		"DescribeCompShareInstance",
		"GetCompShareInstancePrice",
		"CheckCompShareResourceCapacity",
		"DescribeCompShareImages",
		"GetCompShareAccountInfo",
		"GetSoftwareUrl",
		"DescribeCompShareSoftwarePort",
		"DownloadTeamOrder",
	}
	for _, action := range l0Actions {
		level, err := Check(action)
		if err != nil {
			t.Errorf("Check(%q) unexpected error: %v", action, err)
		}
		if level != L0 {
			t.Errorf("Check(%q) = %v, want L0", action, level)
		}
	}
}

func TestCheck_L1_NeedConfirmation(t *testing.T) {
	l1Actions := []string{
		"CreateCompShareInstance",
		"StartCompShareInstance",
		"StopCompShareInstance",
		"RebootCompShareInstance",
		"ResizeCompShareInstance",
		"ResetCompShareInstancePassword",
		"ReinstallCompShareInstance",
		"UpdateCompShareStopScheduler",
		"DeleteCompShareStopScheduler",
	}
	for _, action := range l1Actions {
		level, err := Check(action)
		if err != nil {
			t.Errorf("Check(%q) unexpected error: %v", action, err)
		}
		if level != L1 {
			t.Errorf("Check(%q) = %v, want L1", action, level)
		}
	}
}

func TestCheck_L2_Refuse(t *testing.T) {
	l2Actions := []string{
		"TerminateCompShareInstance",
		"TerminateCompShareCustomImage",
		"DeleteCompshareDisk",
		"DeleteCompShareTeam",
	}
	for _, action := range l2Actions {
		level, err := Check(action)
		if err != nil {
			t.Errorf("Check(%q) unexpected error: %v", action, err)
		}
		if level != L2 {
			t.Errorf("Check(%q) = %v, want L2", action, level)
		}
	}
}

func TestCheck_UnknownAction_Rejected(t *testing.T) {
	unknowns := []string{
		"DropDatabase",
		"InternalAdminAPI",
		"",
		"describecompshareinstance", // case sensitive
	}
	for _, action := range unknowns {
		_, err := Check(action)
		if err == nil {
			t.Errorf("Check(%q) expected error for unknown action, got nil", action)
		}
	}
}

func TestCheck_WhitelistCompleteness(t *testing.T) {
	// Verify we have all three levels represented
	var l0, l1, l2 int
	for _, level := range ActionLevels {
		switch level {
		case L0:
			l0++
		case L1:
			l1++
		case L2:
			l2++
		}
	}
	if l0 == 0 || l1 == 0 || l2 == 0 {
		t.Errorf("whitelist missing levels: L0=%d, L1=%d, L2=%d", l0, l1, l2)
	}
	total := l0 + l1 + l2
	if total < 60 {
		t.Errorf("whitelist too small: got %d actions, want >= 60", total)
	}
}
