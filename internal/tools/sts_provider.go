package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Credentials holds temporary STS credentials returned by AssumeRole.
type Credentials struct {
	AccessKeyId     string
	AccessKeySecret string
	SecurityToken   string
	ExpireAt        time.Time
}

// CredentialProvider abstracts how credentials are obtained.
type CredentialProvider interface {
	Get(ctx context.Context) (*Credentials, error)
}

// StaticCredentialProvider is a CredentialProvider that always returns the
// same fixed *Credentials. Useful in tests and single-tenant scenarios.
type StaticCredentialProvider struct {
	Cred *Credentials
}

func (s StaticCredentialProvider) Get(_ context.Context) (*Credentials, error) {
	return s.Cred, nil
}

// STSOption is a functional option for NewSTSProvider.
type STSOption func(*STSProvider)

// WithDurationSeconds sets the DurationSeconds parameter sent to AssumeRole.
// When 0 (default), the parameter is omitted and the STS service default applies.
func WithDurationSeconds(d int) STSOption {
	return func(p *STSProvider) {
		p.durationSeconds = d
	}
}

// WithRefreshBefore sets how early credentials are renewed before expiry.
// Default is 5 minutes.
func WithRefreshBefore(d time.Duration) STSOption {
	return func(p *STSProvider) {
		p.refreshBefore = d
	}
}

// STSProvider calls the UCloud STS AssumeRole API to obtain temporary
// credentials. Credentials are cached per RoleUrn and refreshed proactively
// before expiry. Concurrent requests for the same RoleUrn are deduplicated.
type STSProvider struct {
	serviceAK, serviceSK, stsURL string
	httpClient                   *http.Client
	refreshBefore                time.Duration
	durationSeconds              int

	mu       sync.Mutex
	cache    map[string]*Credentials
	inflight map[string]chan struct{}
}

// NewSTSProvider creates an STSProvider that signs AssumeRole requests with
// serviceAK / serviceSK and posts them to stsURL.
func NewSTSProvider(serviceAK, serviceSK, stsURL string, opts ...STSOption) *STSProvider {
	p := &STSProvider{
		serviceAK:     serviceAK,
		serviceSK:     serviceSK,
		stsURL:        stsURL,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		refreshBefore: 5 * time.Minute,
		cache:         make(map[string]*Credentials),
		inflight:      make(map[string]chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Get retrieves credentials for the UserContext stored in ctx.
// It returns an error when the context carries no UserContext or the RoleUrn
// is empty.
func (p *STSProvider) Get(ctx context.Context) (*Credentials, error) {
	u, ok := UserFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("no user in context (use tools.WithUser)")
	}
	if u.RoleUrn == "" {
		return nil, fmt.Errorf("UserContext.RoleUrn is empty")
	}

	p.mu.Lock()
	if c, hit := p.cache[u.RoleUrn]; hit && time.Until(c.ExpireAt) > p.refreshBefore {
		p.mu.Unlock()
		return c, nil
	}
	if ch, inFlight := p.inflight[u.RoleUrn]; inFlight {
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// Re-check cache after inflight completes.
		return p.Get(ctx)
	}
	ch := make(chan struct{})
	p.inflight[u.RoleUrn] = ch
	p.mu.Unlock()

	// Ensure inflight is cleared and channel is closed when done.
	var cred *Credentials
	var assumeErr error
	defer func() {
		p.mu.Lock()
		if assumeErr == nil && cred != nil {
			p.cache[u.RoleUrn] = cred
		}
		delete(p.inflight, u.RoleUrn)
		close(ch)
		p.mu.Unlock()
	}()

	cred, assumeErr = p.assumeRole(ctx, u)
	return cred, assumeErr
}

func (p *STSProvider) assumeRole(ctx context.Context, u UserContext) (*Credentials, error) {
	session := u.SessionName
	if session == "" {
		session = "agent-default"
	}
	params := map[string]string{
		"Action":          "AssumeRole",
		"RoleUrn":         u.RoleUrn,
		"RoleSessionName": session,
		"PublicKey":       p.serviceAK,
	}
	if p.durationSeconds > 0 {
		params["DurationSeconds"] = fmt.Sprintf("%d", p.durationSeconds)
	}
	params["Signature"] = ucloudSign(params, p.serviceSK)

	body, err := postForm(ctx, p.httpClient, p.stsURL, params)
	if err != nil {
		return nil, fmt.Errorf("AssumeRole HTTP: %w", err)
	}

	var resp struct {
		RetCode     int    `json:"RetCode"`
		Message     string `json:"Message"`
		Credentials struct {
			AccessKeyId     string `json:"AccessKeyId"`
			AccessKeySecret string `json:"AccessKeySecret"`
			SecurityToken   string `json:"SecurityToken"`
			Expiration      string `json:"Expiration"`
		} `json:"Credentials"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("AssumeRole parse: %w", err)
	}
	if resp.RetCode != 0 {
		return nil, fmt.Errorf("AssumeRole RetCode=%d: %s", resp.RetCode, resp.Message)
	}

	exp, err := time.Parse(time.RFC3339, resp.Credentials.Expiration)
	if err != nil {
		// Fallback: avoid permanent cache miss if Expiration is malformed.
		exp = time.Now().Add(55 * time.Minute)
	}

	return &Credentials{
		AccessKeyId:     resp.Credentials.AccessKeyId,
		AccessKeySecret: resp.Credentials.AccessKeySecret,
		SecurityToken:   resp.Credentials.SecurityToken,
		ExpireAt:        exp,
	}, nil
}

// postForm encodes params as application/x-www-form-urlencoded and POSTs to
// target. The raw response body is returned.
func postForm(ctx context.Context, client *http.Client, target string, params map[string]string) ([]byte, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 1 << 20 // 1 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}
