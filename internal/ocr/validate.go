package ocr

import (
	"encoding/base64"
	"fmt"
	"strings"
)

var allowedImageFormats = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// ValidateImageDataURL checks that dataURL is a well-formed base64-encoded
// image with an allowed MIME type and decoded size within maxBytes.
// Returns the MIME type on success.
func ValidateImageDataURL(dataURL string, maxBytes int) (format string, err error) {
	const prefix = "data:"
	const encoding = ";base64,"

	if !strings.HasPrefix(dataURL, prefix) {
		return "", fmt.Errorf("expected data URL (data:image/...;base64,...)")
	}

	encIdx := strings.Index(dataURL, encoding)
	if encIdx < 0 {
		return "", fmt.Errorf("expected base64 encoding (;base64,)")
	}

	format = dataURL[len(prefix):encIdx]
	if !allowedImageFormats[format] {
		return "", fmt.Errorf("unsupported format %q (allowed: jpeg, png, webp)", format)
	}

	b64Data := dataURL[encIdx+len(encoding):]
	if b64Data == "" {
		return "", fmt.Errorf("empty image data")
	}

	decoded, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return "", fmt.Errorf("invalid base64: %w", err)
	}

	if len(decoded) > maxBytes {
		return "", fmt.Errorf("image %d bytes exceeds limit %d bytes", len(decoded), maxBytes)
	}

	return format, nil
}
