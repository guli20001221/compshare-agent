package ocr

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tenMB = 10 * 1024 * 1024

func makeDataURL(mime string, payload []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(payload)
}

func TestValidateImageDataURL_JPEG(t *testing.T) {
	url := makeDataURL("image/jpeg", []byte("fake-jpeg"))
	format, err := ValidateImageDataURL(url, tenMB)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", format)
}

func TestValidateImageDataURL_PNG(t *testing.T) {
	url := makeDataURL("image/png", []byte("fake-png"))
	format, err := ValidateImageDataURL(url, tenMB)
	require.NoError(t, err)
	assert.Equal(t, "image/png", format)
}

func TestValidateImageDataURL_WebP(t *testing.T) {
	url := makeDataURL("image/webp", []byte("fake-webp"))
	format, err := ValidateImageDataURL(url, tenMB)
	require.NoError(t, err)
	assert.Equal(t, "image/webp", format)
}

func TestValidateImageDataURL_UnsupportedFormat(t *testing.T) {
	url := makeDataURL("image/gif", []byte("fake"))
	_, err := ValidateImageDataURL(url, tenMB)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported format")
}

func TestValidateImageDataURL_NotDataURL(t *testing.T) {
	_, err := ValidateImageDataURL("https://example.com/img.jpg", tenMB)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data URL")
}

func TestValidateImageDataURL_MissingBase64(t *testing.T) {
	_, err := ValidateImageDataURL("data:image/jpeg,raw-data", tenMB)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base64")
}

func TestValidateImageDataURL_ExceedsMaxBytes(t *testing.T) {
	url := makeDataURL("image/jpeg", make([]byte, 200))
	_, err := ValidateImageDataURL(url, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds limit")
}

func TestValidateImageDataURL_InvalidBase64(t *testing.T) {
	_, err := ValidateImageDataURL("data:image/jpeg;base64,not-valid-base64!!!", tenMB)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid base64")
}

func TestValidateImageDataURL_EmptyData(t *testing.T) {
	_, err := ValidateImageDataURL("data:image/jpeg;base64,", tenMB)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}
