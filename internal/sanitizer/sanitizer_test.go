package sanitizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitize_JupyterToken(t *testing.T) {
	result := map[string]any{
		"DataSet": []any{
			map[string]any{
				"UHostId":      "uhost-xxx",
				"JupyterToken": "eyJhbGciOiJIUzI1NiJ9.real-token",
			},
		},
		"RetCode": float64(0),
	}

	sanitized := Sanitize("DescribeCompShareJupyterToken", result)

	// Token should be replaced
	ds := sanitized["DataSet"].([]any)
	first := ds[0].(map[string]any)
	assert.Equal(t, "[已获取,请通过安全通道查看]", first["JupyterToken"])

	// Non-sensitive fields unchanged
	assert.Equal(t, "uhost-xxx", first["UHostId"])
	assert.Equal(t, float64(0), sanitized["RetCode"])
}

func TestSanitize_TopLevelJupyterToken(t *testing.T) {
	result := map[string]any{
		"JupyterToken": "secret-token-value",
		"RetCode":      float64(0),
	}

	sanitized := Sanitize("DescribeCompShareJupyterToken", result)
	assert.Equal(t, "[已获取,请通过安全通道查看]", sanitized["JupyterToken"])
}

func TestSanitize_Password(t *testing.T) {
	result := map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "dGVzdDEyMw==",
		"RetCode":  float64(0),
	}

	sanitized := Sanitize("ResetCompShareInstancePassword", result)
	assert.Equal(t, "[已设置]", sanitized["Password"])
	assert.Equal(t, "uhost-xxx", sanitized["UHostId"])
}

func TestSanitize_GenericPattern(t *testing.T) {
	result := map[string]any{
		"PrivateKey": "ssh-rsa AAAA...",
		"SecretKey":  "sk-xxx",
		"AccessKey":  "ak-xxx",
		"PublicIP":   "1.2.3.4",
	}

	sanitized := Sanitize("SomeAction", result)
	assert.Equal(t, "[REDACTED]", sanitized["PrivateKey"])
	assert.Equal(t, "[REDACTED]", sanitized["SecretKey"])
	assert.Equal(t, "[REDACTED]", sanitized["AccessKey"])
	assert.Equal(t, "1.2.3.4", sanitized["PublicIP"]) // not sensitive
}

func TestSanitize_GenericPattern_LowercaseAndSnakeCase(t *testing.T) {
	result := map[string]any{
		"password":     "secret123",
		"access_token": "tok-xxx",
		"private_key":  "key-xxx",
		"secret_key":   "sk-xxx",
		"accesskey":    "ak-xxx",
		"PublicIP":     "1.2.3.4",
	}

	sanitized := Sanitize("SomeAction", result)
	assert.Equal(t, "[REDACTED]", sanitized["password"])
	assert.Equal(t, "[REDACTED]", sanitized["access_token"])
	assert.Equal(t, "[REDACTED]", sanitized["private_key"])
	assert.Equal(t, "[REDACTED]", sanitized["secret_key"])
	assert.Equal(t, "[REDACTED]", sanitized["accesskey"])
	assert.Equal(t, "1.2.3.4", sanitized["PublicIP"])
}

func TestSanitize_NonSensitiveUnchanged(t *testing.T) {
	result := map[string]any{
		"UHostId": "uhost-xxx",
		"State":   "Running",
		"GpuType": "4090",
	}

	sanitized := Sanitize("DescribeCompShareInstance", result)
	assert.Equal(t, "uhost-xxx", sanitized["UHostId"])
	assert.Equal(t, "Running", sanitized["State"])
	assert.Equal(t, "4090", sanitized["GpuType"])
}

func TestSanitize_DeepCopy(t *testing.T) {
	original := map[string]any{
		"DataSet": []any{
			map[string]any{
				"JupyterToken": "real-token",
				"UHostId":      "uhost-xxx",
			},
		},
	}

	_ = Sanitize("DescribeCompShareJupyterToken", original)

	// Original must NOT be modified
	ds := original["DataSet"].([]any)
	first := ds[0].(map[string]any)
	assert.Equal(t, "real-token", first["JupyterToken"])
}

func TestSanitize_NilResult(t *testing.T) {
	assert.Nil(t, Sanitize("anything", nil))
}

func TestSanitizeArgs_Password(t *testing.T) {
	args := map[string]any{
		"UHostId":  "uhost-xxx",
		"Password": "MySecret123",
		"Zone":     "cn-wlcb-01",
	}

	sanitized := SanitizeArgs("ResetCompShareInstancePassword", args)
	assert.Equal(t, "[REDACTED]", sanitized["Password"])
	assert.Equal(t, "uhost-xxx", sanitized["UHostId"])
	assert.Equal(t, "cn-wlcb-01", sanitized["Zone"])

	// Original unchanged
	assert.Equal(t, "MySecret123", args["Password"])
}

func TestSanitizeArgs_NilArgs(t *testing.T) {
	assert.Nil(t, SanitizeArgs("anything", nil))
}

func TestExtractJupyterToken_TopLevel(t *testing.T) {
	result := map[string]any{
		"JupyterToken": "token-123",
	}
	assert.Equal(t, "token-123", ExtractJupyterToken(result))
}

func TestExtractJupyterToken_InDataSet(t *testing.T) {
	result := map[string]any{
		"DataSet": []any{
			map[string]any{
				"JupyterToken": "token-456",
			},
		},
	}
	assert.Equal(t, "token-456", ExtractJupyterToken(result))
}

func TestExtractJupyterToken_NotFound(t *testing.T) {
	result := map[string]any{
		"RetCode": float64(0),
	}
	assert.Equal(t, "", ExtractJupyterToken(result))
}

func TestExtractJupyterToken_Nil(t *testing.T) {
	assert.Equal(t, "", ExtractJupyterToken(nil))
}
