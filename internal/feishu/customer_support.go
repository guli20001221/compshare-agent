package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const maxCustomerSupportQRBytes = 10 << 20

func loadCustomerSupportQR(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent.feishu.customer_support_qr_path: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("agent.feishu.customer_support_qr_path must not be empty")
	}
	if len(data) > maxCustomerSupportQRBytes {
		return nil, fmt.Errorf("agent.feishu.customer_support_qr_path exceeds Feishu's 10 MiB upload limit")
	}
	switch http.DetectContentType(data) {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return data, nil
	default:
		return nil, fmt.Errorf("agent.feishu.customer_support_qr_path must be JPEG, PNG, WebP, or GIF")
	}
}

func (s *Service) uploadMessageImage(ctx context.Context, data []byte, options ...larkcore.RequestOptionFunc) (string, error) {
	req := larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType(larkim.CreateImageImageTypeMessage).
			Image(bytes.NewReader(data)).
			Build()).
		Build()
	resp, err := s.api.Im.V1.Image.Create(ctx, req, options...)
	if err != nil {
		return "", fmt.Errorf("upload Feishu customer-support QR: %w", err)
	}
	if resp == nil {
		return "", fmt.Errorf("upload Feishu customer-support QR: empty response")
	}
	if !resp.Success() {
		return "", fmt.Errorf("upload Feishu customer-support QR: code=%d message=%s request_id=%s", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil || resp.Data.ImageKey == nil || strings.TrimSpace(*resp.Data.ImageKey) == "" {
		return "", fmt.Errorf("upload Feishu customer-support QR: empty image_key")
	}
	return strings.TrimSpace(*resp.Data.ImageKey), nil
}

func customerSupportPostContent(markdown, imageKey string) (string, error) {
	type element struct {
		Tag      string `json:"tag"`
		Text     string `json:"text,omitempty"`
		ImageKey string `json:"image_key,omitempty"`
	}
	content := struct {
		ZhCN struct {
			Content [][]element `json:"content"`
		} `json:"zh_cn"`
	}{}
	content.ZhCN.Content = [][]element{
		{{Tag: "md", Text: markdown}},
		{{Tag: "img", ImageKey: imageKey}},
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Service) replyCustomerSupport(ctx context.Context, messageID string) error {
	if len(s.customerSupportQR) == 0 {
		return s.reply(ctx, messageID, customerSupportReply())
	}
	imageKey, err := s.uploadMessageImage(ctx, s.customerSupportQR)
	if err != nil {
		log.Printf("warning: Feishu customer-support QR upload failed message=%s: %v", messageID, err)
		return s.reply(ctx, messageID, customerSupportImageFallbackReply())
	}
	content, err := customerSupportPostContent(customerSupportReply(), imageKey)
	if err != nil {
		return err
	}
	return s.replyPost(ctx, messageID, content, "customer-support")
}
