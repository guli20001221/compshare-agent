// Package platform holds value objects shared by typed read capabilities and
// their projections. It is a dependency-free leaf package.
package platform

// TargetRefType classifies how a target instance reference was expressed.
type TargetRefType string

const (
	TargetRefFilter           TargetRefType = "filter"
	TargetRefName             TargetRefType = "name"
	TargetRefUHostIDUserInput TargetRefType = "uhost_id_user_input"
)

// TargetRef is a structured pointer to one or more instances. Capabilities read
// its fields directly; they never receive the user's raw sentence.
type TargetRef struct {
	Type  TargetRefType `json:"type"`
	Value string        `json:"value"`
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
