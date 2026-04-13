package prompt

// UserStage represents the user's lifecycle stage.
type UserStage int

const (
	NewUser      UserStage = iota // No instances
	ActiveUser                    // Has running instances
	InactiveUser                  // Has instances but none running
)

// Suggestion is a clickable recommendation chip.
type Suggestion struct {
	Text string `json:"text"`
}

// ClassifyUser determines user stage from instance API result.
func ClassifyUser(apiResult map[string]any) UserStage {
	hosts, ok := apiResult["UHostSet"].([]any)
	if !ok || len(hosts) == 0 {
		return NewUser
	}
	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if state, _ := host["State"].(string); state == "Running" {
			return ActiveUser
		}
	}
	return InactiveUser
}

// GetSuggestions returns personalized opening recommendations.
func GetSuggestions(stage UserStage) []Suggestion {
	switch stage {
	case NewUser:
		return []Suggestion{
			{Text: "帮我推荐一个入门配置"},
			{Text: "平台怎么用"},
			{Text: "查看 GPU 型号和价格"},
		}
	case ActiveUser:
		return []Suggestion{
			{Text: "查看实例状态"},
			{Text: "设置定时关机"},
			{Text: "这个月花了多少钱"},
		}
	case InactiveUser:
		return []Suggestion{
			{Text: "开机"},
			{Text: "查看余额"},
			{Text: "查看 GPU 型号和价格"},
		}
	default:
		return []Suggestion{
			{Text: "帮我推荐一个入门配置"},
		}
	}
}
