package workflow

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// ResetPasswordDef returns the 4-step workflow definition for resetting
// an instance password: query state, confirm, reset, verify.
// Key constraint: non-container instances must be Stopped; containers
// support online reset (Running or Stopped).
func ResetPasswordDef() *Definition {
	return &Definition{
		Name:        "ResetPasswordWorkflow",
		Description: "查询实例 → 确认重置 → 重置密码 → 确认完成",
		Steps: []Step{
			stepQueryForReset(),
			stepConfirmReset(),
			stepResetPassword(),
			stepVerifyReset(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions
// ---------------------------------------------------------------------------

func stepQueryForReset() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			if state == "" {
				return false, "未找到该实例。"
			}
			instanceType := extractInstanceType(result)

			if instanceType == "Container" {
				// Container supports online reset: Running or Stopped
				if state == "Running" || state == "Stopped" {
					return true, ""
				}
				return false, "容器实例当前状态为「" + state + "」，仅 Running 或 Stopped 状态可重置密码。"
			}

			// Non-container (VM): only Stopped
			if state == "Stopped" {
				return true, ""
			}
			if state == "Running" {
				return false, "普通主机需要先关机才能重置密码。请先执行关机操作。"
			}
			return false, "实例当前状态为「" + state + "」，不支持重置密码。"
		},
	}
}

func stepConfirmReset() Step {
	return Step{
		Name: "确认重置",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["Password"] = "[已设置,不显示]"
			summary["warning"] = "密码要求：8-32字符，至少包含2种字符类型（大小写字母/数字/特殊字符）。"
			return summary, nil
		},
	}
}

func stepResetPassword() Step {
	return Step{
		Name: "重置密码",
		Type: StepToolCall,
		Tool: "ResetCompShareInstancePassword",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			password, _ := wfCtx.Params["Password"].(string)
			if password == "" {
				return nil, fmt.Errorf("密码不能为空")
			}
			// Per API docs: container instances enforce local password rules;
			// non-container instances delegate validation to the UHost API.
			instanceType := extractInstanceType(wfCtx.Result("查询实例"))
			if instanceType == "Container" {
				if err := validateContainerPassword(password); err != nil {
					return nil, err
				}
			}
			return map[string]any{
				"Zone":     extractInstanceZone(wfCtx.Result("查询实例"), defaultZone),
				"UHostId":  wfCtx.Params["UHostId"],
				"Password": base64.StdEncoding.EncodeToString([]byte(password)),
			}, nil
		},
	}
}

func stepVerifyReset() Step {
	return Step{
		Name: "确认完成",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// containerSpecialChars is the allowed special character set for container
// instance passwords, per ResetCompShareInstancePassword.md.
const containerSpecialChars = "()`~!@#$%^&*-+=_|{}[]:;'<>,.?/"

// validateContainerPassword checks the container password policy from API docs:
//   - length 8-32 characters
//   - at least 2 character classes (uppercase / lowercase / digit / special)
//   - only [A-Z][a-z][0-9] and the documented special characters are allowed
func validateContainerPassword(password string) error {
	if len(password) < 8 || len(password) > 32 {
		return fmt.Errorf("密码长度必须在 8-32 个字符之间（当前 %d 个字符）", len(password))
	}
	var hasUpper, hasLower, hasDigit, hasSpecial bool
	for _, r := range password {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case strings.ContainsRune(containerSpecialChars, r):
			hasSpecial = true
		default:
			return fmt.Errorf("密码包含不允许的字符「%c」，容器实例仅允许字母、数字及 %s", r, containerSpecialChars)
		}
	}
	classes := 0
	for _, has := range []bool{hasUpper, hasLower, hasDigit, hasSpecial} {
		if has {
			classes++
		}
	}
	if classes < 2 {
		return fmt.Errorf("密码必须至少包含 2 种字符类型（大小写字母/数字/特殊字符），当前仅包含 %d 种", classes)
	}
	return nil
}

// extractInstanceType returns the InstanceType field from the first UHostSet entry.
func extractInstanceType(result map[string]any) string {
	if result == nil {
		return ""
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return ""
	}
	first, ok := hostSet[0].(map[string]any)
	if !ok {
		return ""
	}
	if t, ok := first["InstanceType"].(string); ok {
		return t
	}
	return ""
}
