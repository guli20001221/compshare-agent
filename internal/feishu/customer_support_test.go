package feishu

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBundledCustomerSupportQRIsAValidUploadImage(t *testing.T) {
	data, err := loadCustomerSupportQR(filepath.Join("..", "..", "deploy", "assets", "customer-support-wecom.jpg"))
	require.NoError(t, err)
	require.NotEmpty(t, data)
	require.Equal(t, "image/jpeg", http.DetectContentType(data))
}

func TestEmptyCustomerSupportQRPathKeepsTextOnlyCompatibility(t *testing.T) {
	data, err := loadCustomerSupportQR("")
	require.NoError(t, err)
	require.Nil(t, data)
}
