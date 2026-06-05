package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UAccountClient calls the UCloud internal UAccount API for IAM operations
// (IGetRole, ICreateServiceLinkedRole). The internal URL is reachable only from
// within the UCloud private network; authentication is by network trust, not
// AK/SK signing.
type UAccountClient struct {
	url        string
	httpClient *http.Client
}

// NewUAccountClient creates a client pointed at the internal UAccount URL.
// The URL must be the full endpoint (e.g. "http://internal-iam.example/").
func NewUAccountClient(url string) *UAccountClient {
	return &UAccountClient{
		url:        url,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ErrRoleNotExist is returned by IGetRole when the role does not exist under
// the given company. Callers typically respond by calling ICreateServiceLinkedRole
// and retrying.
var ErrRoleNotExist = errors.New("role not exist")

// IGetRole calls UAccount.IGetRole and returns the role's RoleURN.
// Returns ErrRoleNotExist when the response indicates the role is missing;
// other non-zero RetCodes return a wrapped error.
func (c *UAccountClient) IGetRole(ctx context.Context, companyID uint32, roleName string) (string, error) {
	body, err := c.post(ctx, map[string]any{
		"Backend":   "UAccount",
		"Action":    "IGetRole",
		"CompanyId": companyID,
		"RoleName":  roleName,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		RetCode int    `json:"RetCode"`
		Message string `json:"Message"`
		Role    struct {
			RoleName string `json:"RoleName"`
			RoleURN  string `json:"RoleURN"`
		} `json:"Role"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("IGetRole parse: %w", err)
	}
	if resp.RetCode != 0 {
		if strings.Contains(resp.Message, "Role Not Exist") {
			return "", ErrRoleNotExist
		}
		return "", fmt.Errorf("IGetRole RetCode=%d: %s", resp.RetCode, resp.Message)
	}
	if resp.Role.RoleURN == "" {
		return "", ErrRoleNotExist
	}
	return resp.Role.RoleURN, nil
}

// ICreateServiceLinkedRole calls UAccount.ICreateServiceLinkedRole to provision
// the named service-linked role under companyID. The call is idempotent on the
// UAccount side: a follow-up call after the role already exists returns success.
func (c *UAccountClient) ICreateServiceLinkedRole(ctx context.Context, companyID uint32, serviceName string) error {
	body, err := c.post(ctx, map[string]any{
		"Backend":     "UAccount",
		"Action":      "ICreateServiceLinkedRole",
		"CompanyId":   companyID,
		"ServiceName": serviceName,
	})
	if err != nil {
		return err
	}
	var resp struct {
		RetCode int    `json:"RetCode"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("ICreateServiceLinkedRole parse: %w", err)
	}
	if resp.RetCode != 0 {
		return fmt.Errorf("ICreateServiceLinkedRole RetCode=%d: %s", resp.RetCode, resp.Message)
	}
	return nil
}

func (c *UAccountClient) post(ctx context.Context, params map[string]any) ([]byte, error) {
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("uaccount marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("uaccount build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("uaccount http: %w", err)
	}
	defer resp.Body.Close()
	const maxResponseSize = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("uaccount read: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("uaccount status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
