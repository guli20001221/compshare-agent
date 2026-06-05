package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// stsRetCodeRoleNotExist is the UCloud STS error code returned by AssumeRole
// when the requested RoleUrn does not exist in the target company. We treat
// this as the trigger for the service-linked role bootstrap path: the role is
// created on demand, then AssumeRole is retried once.
const stsRetCodeRoleNotExist = 11277

// AssumeRoleError is returned by STSProvider when the STS service responds with
// a non-zero RetCode. Callers (including STSProvider's own recovery path) can
// use errors.As to inspect the RetCode without parsing the error string.
type AssumeRoleError struct {
	RetCode int
	Message string
}

func (e *AssumeRoleError) Error() string {
	return fmt.Sprintf("AssumeRole RetCode=%d: %s", e.RetCode, e.Message)
}

// IsRoleNotExist reports whether err is an AssumeRoleError with RetCode 11277.
func IsRoleNotExist(err error) bool {
	var are *AssumeRoleError
	return errors.As(err, &are) && are.RetCode == stsRetCodeRoleNotExist
}

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

// WithRoleBootstrapper installs a RoleBootstrapper that recovers from
// AssumeRole RetCode=11277 (RoleNotExist) by provisioning the per-tenant role
// on demand and retrying AssumeRole once. When unset, RoleNotExist errors are
// returned to the caller unchanged.
func WithRoleBootstrapper(b RoleBootstrapper) STSOption {
	return func(p *STSProvider) {
		p.bootstrapper = b
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
	bootstrapper                 RoleBootstrapper

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
	if assumeErr != nil && IsRoleNotExist(assumeErr) && u.TopOrganizationID != 0 {
		// Recovery: provision the service-linked role on demand and retry once.
		// Surface bootstrap state in the returned error so operators can tell
		// "auto-provision not configured" / "auto-provision tried, failed"
		// / "auto-provision succeeded, still failed" apart in trace logs.
		switch {
		case p.bootstrapper == nil:
			assumeErr = fmt.Errorf("%w (auto-provision not configured: set agent.sts.iam_url to enable)", assumeErr)
		default:
			bErr := p.bootstrapper.Bootstrap(ctx, u.TopOrganizationID)
			if bErr != nil {
				assumeErr = fmt.Errorf("%w (auto-provision via UAccount failed for company=%d: %v)", assumeErr, u.TopOrganizationID, bErr)
			} else {
				cred, assumeErr = p.assumeRole(ctx, u)
				if assumeErr != nil {
					assumeErr = fmt.Errorf("%w (retry after successful service-linked role bootstrap for company=%d still failed)", assumeErr, u.TopOrganizationID)
				}
			}
		}
	}
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
		return nil, &AssumeRoleError{RetCode: resp.RetCode, Message: resp.Message}
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
