package workflow

import "fmt"

// Price is a snapshot, not a quote. Upstream reports compute, disks and
// paid image as separate components, so an amount is assembled rather than
// read from one field, and every displayed figure carries the estimate
// suffix. The sealed draft keeps the snapshot it was confirmed with; the
// final charge is whatever settlement produces.

// priceAmountFor reads the amount quoted for one charge type out of one of the
// price arrays.
//
// Upstream reports compute, disks and paid image as separate components. Disks
// already includes SystemDisks, so the latter is used only as a compatibility
// fallback when Disks is absent. Price is the legacy all-in fallback.
func priceAmountFor(raw map[string]any, arrKey, chargeType string) (float64, bool) {
	arr, ok := raw[arrKey].([]any)
	if !ok {
		return 0, false
	}
	for _, entry := range arr {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if ct, _ := m["ChargeType"].(string); ct != chargeType {
			continue
		}
		total := float64(0)
		hasComponent := false
		for _, key := range []string{"Instance", "CompShareImage"} {
			if n, ok := priceNumber(m[key]); ok {
				total += n
				hasComponent = true
			}
		}
		if n, ok := priceNumber(m["Disks"]); ok {
			total += n
			hasComponent = true
		} else if n, ok := priceNumber(m["SystemDisks"]); ok {
			total += n
			hasComponent = true
		}
		if hasComponent {
			return total, true
		}
		if n, ok := priceNumber(m["Price"]); ok {
			return n, true
		}
	}
	return 0, false
}

// priceListAmountFor reads the undiscounted price, which upstream reports under
// either name depending on the endpoint.
func priceListAmountFor(raw map[string]any, chargeType string) (float64, bool) {
	if list, ok := priceAmountFor(raw, "ListPriceDetails", chargeType); ok {
		return list, true
	}
	return priceAmountFor(raw, "OriginalPriceDetails", chargeType)
}

// estimatedPriceSuffix marks the quote as an estimate in the card's price VALUE,
// not only in a separate note field.
//
// The value is where it has to be. The HTTP confirmation frame hands these args to
// a frontend this repo does not own, which renders them with its own labels; a
// structured flag alone would be honest only once that frontend adopts it, and
// until then the user would read a bare number as a commitment. Upstream cannot
// hold a price, so the number is an estimate in every renderer or it is misleading
// in some of them.
const estimatedPriceSuffix = "（预估）"

// createPriceNote is the fuller sentence, carried as its own card field so a
// renderer can place it properly beneath the price.
const createPriceNote = "最终费用以实际创建和结算结果为准"

// extractEstimatedPrice builds the snapshot of what the user is about to be
// quoted, and renders the one string that both the card and the seal will carry.
//
// It is the only place a create price is turned into text, so the card and seal
// cannot render different quotes.
//
// Returns nil when upstream quoted nothing usable for this charge type — the card
// then shows no price rather than a fabricated one, because a 0 renders as free.
//
// It records only what upstream said. No quote id (there is none — SourceRequestID
// is the response's request_uuid and is named for what it is), no validity, no
// currency, and Locked=false because the platform cannot hold this number.
func extractEstimatedPrice(priceResult any, chargeType string) *EstimatedPriceSnapshot {
	raw, ok := priceResult.(map[string]any)
	if !ok {
		return nil
	}
	payable, ok := priceAmountFor(raw, "PriceDetails", chargeType)
	if !ok {
		return nil
	}
	text := fmt.Sprintf("¥%.2f%s", payable, chargePeriodUnit(chargeType))
	snapshot := &EstimatedPriceSnapshot{
		ChargeType:      chargeType,
		PayableAmount:   payable,
		SourceRequestID: paramStr(raw, "request_uuid", ""),
		Locked:          false,
	}
	if list, hasList := priceListAmountFor(raw, chargeType); hasList {
		snapshot.ListAmount = &list
		if list > payable {
			text += fmt.Sprintf("（原价 ¥%.2f）", list)
		}
	}
	snapshot.DisplayText = text + estimatedPriceSuffix
	return snapshot
}

// chargePeriodUnit maps a ChargeType to its billing-period suffix for display.
func chargePeriodUnit(chargeType string) string {
	switch chargeType {
	case "Day":
		return "/天"
	case "Month":
		return "/月"
	case "Year":
		return "/年"
	case "Dynamic":
		return "/小时"
	default: // Postpay / Spot are pay-as-you-go hourly
		return "/小时"
	}
}

// priceNumber coerces a JSON-decoded numeric price field to float64.
func priceNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}
