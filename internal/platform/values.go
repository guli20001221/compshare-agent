// Package platform holds value objects shared by typed read capabilities and
// their projections. It is a dependency-free leaf package.
package platform

// TargetRefType classifies how a target instance reference was expressed.
type TargetRefType string

const (
	TargetRefFilter           TargetRefType = "filter"
	TargetRefName             TargetRefType = "name"
	TargetRefUHostIDUserInput TargetRefType = "uhost_id_user_input"
	TargetRefSlotPosition     TargetRefType = "slot_position"
)

// TargetRefTypeValues is the enum's single source of allowed wire values, in
// declaration order. The capability tool schema and the runtime validator both
// read it, so the model-facing enum and the accepted set never drift.
func TargetRefTypeValues() []string {
	return []string{
		string(TargetRefFilter), string(TargetRefName),
		string(TargetRefUHostIDUserInput), string(TargetRefSlotPosition),
	}
}

// TargetSource records whether a reference came from the current user turn or a
// prior turn. It is a provenance marker, never re-parsed from raw text by a
// capability handler.
type TargetSource string

const (
	SourceUserText  TargetSource = "user_text"
	SourcePriorTurn TargetSource = "prior_turn"
)

// TargetSourceValues is the enum's single source of allowed wire values.
func TargetSourceValues() []string {
	return []string{string(SourceUserText), string(SourcePriorTurn)}
}

// TargetRef is a structured pointer to one or more instances. Capabilities read
// its fields directly; they never receive the user's raw sentence.
type TargetRef struct {
	Type       TargetRefType `json:"type"`
	Value      string        `json:"value"`
	Source     TargetSource  `json:"source,omitempty"`
	SourceSpan string        `json:"source_span,omitempty"`
}

// Metric is a monitor dimension.
type Metric string

const (
	MetricCPU    Metric = "cpu"
	MetricMemory Metric = "memory"
	MetricGPU    Metric = "gpu"
	MetricVRAM   Metric = "vram"
)

// MetricValues is the enum's single source of allowed wire values.
func MetricValues() []string {
	return []string{string(MetricCPU), string(MetricMemory), string(MetricGPU), string(MetricVRAM)}
}

// TimeWindowType classifies a monitor time window.
type TimeWindowType string

const (
	TimeWindowPreset   TimeWindowType = "preset"
	TimeWindowRelative TimeWindowType = "relative"
	TimeWindowAbsolute TimeWindowType = "absolute"
)

// TimeWindowTypeValues is the enum's single source of allowed wire values.
func TimeWindowTypeValues() []string {
	return []string{string(TimeWindowPreset), string(TimeWindowRelative), string(TimeWindowAbsolute)}
}

// TimeWindow is a structured monitor-history window.
type TimeWindow struct {
	Type     TimeWindowType `json:"type"`
	Preset   string         `json:"preset,omitempty"`
	Amount   int            `json:"amount,omitempty"`
	Unit     string         `json:"unit,omitempty"`
	Start    string         `json:"start,omitempty"`
	End      string         `json:"end,omitempty"`
	Timezone string         `json:"timezone,omitempty"`
	// SourceSpan is the exact current-user text that expressed the time window.
	// The engine verifies it before dispatch, so a model cannot turn “昨天” into
	// an invented absolute date while still producing a schema-valid request.
	SourceSpan string `json:"source_span"`
}

// ImageSource selects which image catalog an image-list capability queries.
type ImageSource string

const (
	ImageSourcePlatform  ImageSource = "platform"
	ImageSourceCustom    ImageSource = "custom"
	ImageSourceCommunity ImageSource = "community"
	ImageSourceShared    ImageSource = "shared"
)

// ImageSourceValues is the enum's single source of allowed wire values.
func ImageSourceValues() []string {
	return []string{
		string(ImageSourcePlatform), string(ImageSourceCustom),
		string(ImageSourceCommunity), string(ImageSourceShared),
	}
}

// ListMode toggles between listing everything and filtering by a query.
type ListMode string

const (
	ListModeAll      ListMode = "all"
	ListModeFiltered ListMode = "filtered"
)

// ListModeValues is the enum's single source of allowed wire values.
func ListModeValues() []string {
	return []string{string(ListModeAll), string(ListModeFiltered)}
}

// PriceKind selects the account (discounted) or catalog (list) price.
type PriceKind string

const (
	PriceKindAccount PriceKind = "account"
	PriceKindCatalog PriceKind = "catalog"
)

// PriceKindValues is the enum's single source of allowed wire values.
func PriceKindValues() []string {
	return []string{string(PriceKindAccount), string(PriceKindCatalog)}
}

// CFSKind distinguishes the four CFS read protocols.
type CFSKind string

const (
	CFSKindList         CFSKind = "list"
	CFSKindCreatePrice  CFSKind = "create_price"
	CFSKindUpgradePrice CFSKind = "upgrade_price"
	CFSKindRefund       CFSKind = "refund"
)

// DetailLevel selects overview vs. full detail for spec queries.
type DetailLevel string

const (
	DetailLevelSummary DetailLevel = "summary"
	DetailLevelFull    DetailLevel = "full"
)

// DetailLevelValues is the enum's single source of allowed wire values.
func DetailLevelValues() []string {
	return []string{string(DetailLevelSummary), string(DetailLevelFull)}
}

// CFSRef is a structured pointer to one CFS filesystem.
type CFSRef struct {
	ID string `json:"id"`
}
