package security

import "fmt"

// Level represents the security level of an API action.
type Level int

const (
	L0 Level = 0 // Read-only, execute directly
	L1 Level = 1 // Mutating, requires user confirmation
	L2 Level = 2 // Destructive, refuse execution
)

func (l Level) String() string {
	switch l {
	case L0:
		return "L0(直接执行)"
	case L1:
		return "L1(需确认)"
	case L2:
		return "L2(拒绝执行)"
	default:
		return fmt.Sprintf("L?(%d)", int(l))
	}
}

// ActionLevels maps every whitelisted Action to its security level.
var ActionLevels = map[string]Level{
	// ── L0: Read-only / Query (42) ──────────────────────────
	"DescribeCompShareInstance":               L0,
	"DescribeCompShareImages":                 L0,
	"DescribeCompShareCustomImages":           L0,
	"DescribeCompShareSharingImages":          L0,
	"DescribeFavoriteImages":                  L0,
	"DescribeCommunityImages":                 L0,
	"DescribeCompShareSupportZone":            L0,
	"DescribeAvailableCompShareInstanceTypes": L0,
	"DescribeCompShareMachineTypeFamilies":    L0,
	"CheckCompShareResourceCapacity":          L0,
	"GetCompShareInstancePrice":               L0,
	"GetCompShareInstanceUserPrice":           L0,
	"GetCompShareInstanceUpgradePrice":        L0,
	"GetCompShareAttachedDiskUpgradePrice":    L0,
	"GetCompShareRefundPrice":                 L0,
	"GetCompShareAccountInfo":                 L0,
	"GetCompShareInstanceMonitor":             L0,
	"GetCompShareImageCreateProgress":         L0,
	"DescribeCompShareImageTags":              L0,
	"DescribeCompShareImageShareAccounts":     L0,
	"DescribeCompShareSoftwarePort":           L0,
	"GetSoftwareUrl":                          L0,
	"DescribeCompShareJupyterToken":           L0,
	"CheckCompShareResizeAttachedDisk":        L0,
	"CheckCompShareNetOptimizer":              L0,
	"DescribeModelRepositoryModels":           L0,
	"DescribeModelRepositoryTags":             L0,
	"GetOpenClawModelList":                    L0,
	"GetCompShareTeamInfo":                    L0,
	"DescribeTeamMemberOrder":                 L0,
	"DescribeTeamMemberOrderCount":            L0,
	"DescribeTeamMemberUnpaidOrder":           L0,
	"DescribeTeamMemberUnpaidOrderCount":      L0,
	"DescribeSelfCommunityImages":             L0,
	"DescribeUserCommunityImages":             L0,
	"CreateUs3StsToken":                       L0,
	"DownloadTeamOrder":                       L0,

	// ── L1: Mutating, requires confirmation (25) ────────────
	"CreateCompShareInstance":          L1,
	"StartCompShareInstance":           L1,
	"StopCompShareInstance":            L1,
	"RebootCompShareInstance":          L1,
	"ResizeCompShareInstance":          L1,
	"ModifyCompShareInstanceName":      L1,
	"ResetCompShareInstancePassword":   L1,
	"CreateAndAttachCompshareDisk":     L1,
	"ResizeCompShareDisk":              L1,
	"CreateCompShareCustomImage":       L1,
	"UpdateCompShareImage":             L1,
	"ModifyCompShareImageShareAccount": L1,
	"UpdateCompShareStopScheduler":     L1,
	"DeleteCompShareStopScheduler":     L1,
	"CreateCompShareTeam":              L1,
	"UpdateCompShareTeam":              L1,
	"CreateCompShareTeamRelation":      L1,
	"SetCompShareTeamRelation":         L1,
	"SetCompShareTeamAmount":           L1,

	// Reinstall moved to L1: destructive (erases system disk) but legitimate
	// user operation with mandatory workflow confirmation.
	"ReinstallCompShareInstance": L1,

	// ── L2: Destructive, always refuse (4) ──────────────────
	"TerminateCompShareInstance":    L2,
	"TerminateCompShareCustomImage": L2,
	"DeleteCompshareDisk":           L2,
	"DeleteCompShareTeam":           L2,
}

// Check returns the security level for an action.
// Returns an error if the action is not in the whitelist.
func Check(action string) (Level, error) {
	level, ok := ActionLevels[action]
	if !ok {
		return -1, fmt.Errorf("不支持的操作: %s", action)
	}
	return level, nil
}
