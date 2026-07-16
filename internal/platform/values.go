// Package platform holds the shared value-object vocabulary used by the agent's
// read capabilities and the legacy intent router. It is a leaf package: it
// imports nothing from internal/intent, so a typed capability can own its
// request contract (internal/capability) without reverse-depending on the intent
// router. internal/intent re-exports every type here via aliases, so existing
// intent.TargetRef / intent.Metric / … references keep compiling unchanged.
package platform

// TargetRefType classifies how a target instance reference was expressed.
type TargetRefType string

const (
	TargetRefFilter           TargetRefType = "filter"
	TargetRefName             TargetRefType = "name"
	TargetRefUHostIDUserInput TargetRefType = "uhost_id_user_input"
	TargetRefSlotPosition     TargetRefType = "slot_position"
)

// TargetSource records whether a reference came from the current user turn or a
// prior turn. It is a provenance marker, never re-parsed from raw text by a
// capability handler.
type TargetSource string

const (
	SourceUserText  TargetSource = "user_text"
	SourcePriorTurn TargetSource = "prior_turn"
)

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

// TimeWindowType classifies a monitor time window.
type TimeWindowType string

const (
	TimeWindowPreset   TimeWindowType = "preset"
	TimeWindowRelative TimeWindowType = "relative"
	TimeWindowAbsolute TimeWindowType = "absolute"
)

// TimeWindow is a structured monitor-history window.
type TimeWindow struct {
	Type  TimeWindowType `json:"type"`
	Value string         `json:"value"`
}

// ImageSource selects which image catalog an image-list capability queries.
type ImageSource string

const (
	ImageSourcePlatform  ImageSource = "platform"
	ImageSourceCustom    ImageSource = "custom"
	ImageSourceCommunity ImageSource = "community"
	ImageSourceShared    ImageSource = "shared"
)

// ListMode toggles between listing everything and filtering by a query.
type ListMode string

const (
	ListModeAll      ListMode = "all"
	ListModeFiltered ListMode = "filtered"
)

// PriceKind selects the account (discounted) or catalog (list) price.
type PriceKind string

const (
	PriceKindAccount PriceKind = "account"
	PriceKindCatalog PriceKind = "catalog"
)

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

// CFSRef is a structured pointer to one CFS filesystem.
type CFSRef struct {
	ID string `json:"id"`
}
