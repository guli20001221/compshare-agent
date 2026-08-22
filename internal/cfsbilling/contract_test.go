package cfsbilling

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewPurchaseTypesExcludeNonOperationalHourlyModes(t *testing.T) {
	got := NewPurchaseTypes()
	assert.Equal(t, []string{Month, Year, Day}, got)
	assert.False(t, SupportsNewPurchase(Dynamic))
	assert.False(t, SupportsNewPurchase(Postpay))

	got[0] = Dynamic
	assert.Equal(t, []string{Month, Year, Day}, NewPurchaseTypes(),
		"callers must not be able to mutate the shared contract")
}

func TestDisplayLabelKeepsLegacyRowsReadableWithoutAdvertisingThem(t *testing.T) {
	assert.Equal(t, "包月", DisplayLabel(Month))
	assert.Equal(t, "旧版按小时计费", DisplayLabel(Dynamic))
	assert.Equal(t, "存量后付费", DisplayLabel(Postpay))
	assert.Equal(t, "future-mode", DisplayLabel("future-mode"))
}
