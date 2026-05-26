package ocr_test

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/ocr"
)

// TestOCRClient_E2E requires LLM_API_KEY and OCR_TEST_IMAGE env vars.
// Run manually: LLM_API_KEY=... OCR_TEST_IMAGE=path/to/screenshot.jpg go test ./internal/ocr -run E2E -v
func TestOCRClient_E2E(t *testing.T) {
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		t.Skip("LLM_API_KEY not set")
	}
	imgPath := os.Getenv("OCR_TEST_IMAGE")
	if imgPath == "" {
		t.Skip("OCR_TEST_IMAGE not set")
	}
	imgBytes, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imgBytes)

	model := os.Getenv("OCR_MODEL")
	if model == "" {
		model = "qwen3-vl-flash"
	}

	client := ocr.NewClient(config.OCRConfig{
		Model:   model,
		BaseURL: "https://api.modelverse.cn/v1",
		APIKey:  apiKey,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	text, err := client.Recognize(ctx, dataURL)
	if err != nil {
		t.Fatalf("OCR failed: %v", err)
	}
	if text == "" {
		t.Fatal("OCR returned empty text")
	}
	t.Logf("model=%s result_length=%d", model, len(text))
	if len(text) > 300 {
		t.Logf("first 300 chars: %.300s", text)
	} else {
		t.Logf("full result: %s", text)
	}
}
