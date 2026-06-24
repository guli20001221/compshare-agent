package guardrails

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractPasswordTurnSecrets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "reset password explicit value",
			in:   "把 uhost-1 的密码重置为 Test@123456789b",
			want: []string{"Test@123456789b"},
		},
		{
			name: "change password to explicit value",
			in:   "把 uhost-1 的密码改成 Test@123456789c",
			want: []string{"Test@123456789c"},
		},
		{
			name: "password question is not a secret",
			in:   "这台机器的密码是多少",
			want: nil,
		},
		{
			name: "short token is ignored",
			in:   "密码为 Ab1",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractPasswordTurnSecrets(tt.in))
		})
	}
}

func TestRedactTurnSecrets(t *testing.T) {
	got := RedactTurnSecrets("密码 Test@123456789b 已更新为 Test@123456789b", []string{"Test@123456789b"})
	assert.NotContains(t, got, "Test@123456789b")
	assert.Contains(t, got, CredentialRedactedOutput)
}
