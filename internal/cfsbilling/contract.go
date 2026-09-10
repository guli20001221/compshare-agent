// Package cfsbilling owns the billing vocabulary for CFS. New-purchase
// options are deliberately separate from historical response compatibility:
// the upstream API can still describe legacy Dynamic/Postpay rows even though
// neither is a currently purchasable CFS billing mode.
package cfsbilling

import "fmt"

const (
	Day     = "Day"
	Month   = "Month"
	Year    = "Year"
	Dynamic = "Dynamic"
	Postpay = "Postpay"
	Spot    = "Spot"
)

// MinSizeGB and MaxSizeGB bound every CFS capacity the upstream API accepts —
// create, resize, and both quotes all run the same validator, so a value outside
// them is refused there rather than priced. They are the single producer of the
// numbers the schemas and the workflow state.
const (
	MinSizeGB = 50
	MaxSizeGB = 2048
)

// SizeInRange reports whether a requested capacity can reach the upstream API at
// all. A caller that skips this hands the model an upstream range error instead
// of a bound it could have read in the tool window.
func SizeInRange(sizeGB int) bool {
	return sizeGB >= MinSizeGB && sizeGB <= MaxSizeGB
}

// SizeRangeHint is the one sentence every CFS capacity parameter appends, so the
// model reads the same bound whichever tool it is filling. It is formatted from
// the constants rather than written out, so the prose cannot drift from what
// SizeInRange enforces.
func SizeRangeHint() string {
	return fmt.Sprintf("容量 %d-%dGB。", MinSizeGB, MaxSizeGB)
}

// NewPurchaseTypes returns the complete public CFS create/quote contract.
// Return a fresh slice so a schema builder cannot mutate process-wide state.
func NewPurchaseTypes() []string {
	return []string{Month, Year, Day}
}

// SupportsNewPurchase reports whether the deployed CFS billing product has a
// working create/price path for this wire value.
func SupportsNewPurchase(chargeType string) bool {
	switch chargeType {
	case Month, Year, Day:
		return true
	default:
		return false
	}
}

// DisplayLabel renders both current purchase modes and legacy response values.
// Dynamic remains readable for existing resources but is never returned by
// NewPurchaseTypes and therefore cannot become a new request option by accident.
func DisplayLabel(chargeType string) string {
	switch chargeType {
	case Day:
		return "包日"
	case Month:
		return "包月"
	case Year:
		return "包年"
	case Dynamic:
		return "旧版按小时计费"
	case Postpay:
		return "存量后付费"
	case Spot:
		return "抢占式"
	default:
		return chargeType
	}
}
