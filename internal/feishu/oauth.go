package feishu

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/compshare-agent/internal/config"
)

const (
	feishuOAuthTokenEndpoint  = "https://open.feishu.cn/open-apis/authen/v2/oauth/token"
	externalImageOAuthPurpose = "external_group_image"
	oauthTokenAAD             = "compshare-agent/feishu/external-image-oauth/v1"
	oauthRefreshSkew          = time.Minute
)

var feishuOAuthHTTPClient = &http.Client{Timeout: 15 * time.Second}

var errExternalImageUserAuthorizationUnavailable = errors.New("external image user authorization unavailable")

// userAccessTokenProvider supplies a token for an internal group member who
// has explicitly authorized this app. It is deliberately narrower than the
// normal bot client and is used only to read message image resources.
type userAccessTokenProvider interface {
	AccessToken(context.Context) (string, error)
}

type oauthToken struct {
	AccessToken           string
	RefreshToken          string
	AccessExpiresAt       time.Time
	RefreshTokenExpiresAt time.Time
}

type oauthTokenStore interface {
	Load(context.Context) (oauthToken, error)
	Save(context.Context, oauthToken) error
}

type tokenCipher struct {
	key [32]byte
}

func newTokenCipher(appID, appSecret string) (tokenCipher, error) {
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(appSecret) == "" {
		return tokenCipher{}, errors.New("Feishu app_id and app_secret are required to protect OAuth tokens")
	}
	// Domain-separate this key from any other use of the app secret. Rotating an
	// app secret intentionally requires a fresh authorization because old token
	// ciphertext can no longer be opened.
	return tokenCipher{key: sha256.Sum256([]byte(oauthTokenAAD + "\x00" + appID + "\x00" + appSecret))}, nil
}

func (c tokenCipher) seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("cannot encrypt an empty OAuth token")
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", fmt.Errorf("create OAuth token cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create OAuth token GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("create OAuth token nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(oauthTokenAAD))
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (c tokenCipher) open(encoded string) (string, error) {
	sealed, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode stored OAuth token: %w", err)
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", fmt.Errorf("create OAuth token cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create OAuth token GCM: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("stored OAuth token is truncated")
	}
	plaintext, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte(oauthTokenAAD))
	if err != nil {
		return "", errors.New("cannot decrypt stored OAuth token; re-authorize the Feishu external-image reader")
	}
	return string(plaintext), nil
}

// PostgresOAuthTokenStore persists rotating user tokens encrypted at rest. The
// table is intentionally dedicated to this integration so normal chat/session
// data never carries an OAuth credential.
type PostgresOAuthTokenStore struct {
	db     *sql.DB
	cipher tokenCipher
}

func NewPostgresOAuthTokenStore(db *sql.DB, appID, appSecret string) (*PostgresOAuthTokenStore, error) {
	if db == nil {
		return nil, errors.New("OAuth token database is required")
	}
	cipher, err := newTokenCipher(appID, appSecret)
	if err != nil {
		return nil, err
	}
	return &PostgresOAuthTokenStore{db: db, cipher: cipher}, nil
}

func VerifyExternalImageOAuthSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("OAuth token database is required")
	}
	rows, err := db.QueryContext(ctx, `SELECT purpose, access_token_ciphertext, refresh_token_ciphertext,
access_expires_at, refresh_token_expires_at FROM feishu_oauth_tokens LIMIT 0`)
	if err != nil {
		return fmt.Errorf("verify schema feishu_oauth_tokens: %w", err)
	}
	return rows.Close()
}

func (s *PostgresOAuthTokenStore) Load(ctx context.Context) (oauthToken, error) {
	var accessCiphertext, refreshCiphertext string
	var accessExpiresAt time.Time
	var refreshExpiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT access_token_ciphertext, refresh_token_ciphertext,
access_expires_at, refresh_token_expires_at
FROM feishu_oauth_tokens WHERE purpose = $1`, externalImageOAuthPurpose).Scan(
		&accessCiphertext, &refreshCiphertext, &accessExpiresAt, &refreshExpiresAt,
	)
	if err != nil {
		return oauthToken{}, err
	}
	accessToken, err := s.cipher.open(accessCiphertext)
	if err != nil {
		return oauthToken{}, err
	}
	refreshToken, err := s.cipher.open(refreshCiphertext)
	if err != nil {
		return oauthToken{}, err
	}
	record := oauthToken{
		AccessToken:     accessToken,
		RefreshToken:    refreshToken,
		AccessExpiresAt: accessExpiresAt,
	}
	if refreshExpiresAt.Valid {
		record.RefreshTokenExpiresAt = refreshExpiresAt.Time
	}
	return record, nil
}

func (s *PostgresOAuthTokenStore) Save(ctx context.Context, record oauthToken) error {
	if record.AccessToken == "" || record.RefreshToken == "" || record.AccessExpiresAt.IsZero() {
		return errors.New("incomplete OAuth token record")
	}
	accessCiphertext, err := s.cipher.seal(record.AccessToken)
	if err != nil {
		return err
	}
	refreshCiphertext, err := s.cipher.seal(record.RefreshToken)
	if err != nil {
		return err
	}
	var refreshExpiresAt any
	if record.RefreshTokenExpiresAt.IsZero() {
		refreshExpiresAt = nil
	} else {
		refreshExpiresAt = record.RefreshTokenExpiresAt
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO feishu_oauth_tokens (
purpose, access_token_ciphertext, refresh_token_ciphertext, access_expires_at, refresh_token_expires_at, updated_at
) VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (purpose) DO UPDATE SET
access_token_ciphertext = EXCLUDED.access_token_ciphertext,
refresh_token_ciphertext = EXCLUDED.refresh_token_ciphertext,
access_expires_at = EXCLUDED.access_expires_at,
refresh_token_expires_at = EXCLUDED.refresh_token_expires_at,
updated_at = now()`, externalImageOAuthPurpose, accessCiphertext, refreshCiphertext,
		record.AccessExpiresAt, refreshExpiresAt)
	if err != nil {
		return fmt.Errorf("save OAuth token: %w", err)
	}
	return nil
}

// ExternalImageTokenProvider refreshes one internal member's delegated token.
// It retains only encrypted tokens durably; BootstrapRefreshToken is used once
// when the row does not exist yet.
type ExternalImageTokenProvider struct {
	appID                 string
	appSecret             string
	bootstrapRefreshToken string
	store                 oauthTokenStore
	client                *http.Client
	tokenEndpoint         string
	now                   func() time.Time

	mu    sync.Mutex
	token oauthToken
}

func NewExternalImageTokenProvider(ctx context.Context, cfg config.FeishuConfig, db *sql.DB) (*ExternalImageTokenProvider, error) {
	store, err := NewPostgresOAuthTokenStore(db, cfg.AppID, cfg.AppSecret)
	if err != nil {
		return nil, err
	}
	return newExternalImageTokenProvider(ctx, cfg, store, feishuOAuthHTTPClient, feishuOAuthTokenEndpoint, time.Now)
}

func newExternalImageTokenProvider(
	ctx context.Context,
	cfg config.FeishuConfig,
	store oauthTokenStore,
	client *http.Client,
	tokenEndpoint string,
	now func() time.Time,
) (*ExternalImageTokenProvider, error) {
	if !cfg.ExternalImageOAuth.Enabled {
		return nil, errors.New("external image OAuth is disabled")
	}
	if store == nil {
		return nil, errors.New("OAuth token store is required")
	}
	if client == nil {
		client = feishuOAuthHTTPClient
	}
	if now == nil {
		now = time.Now
	}
	provider := &ExternalImageTokenProvider{
		appID:                 cfg.AppID,
		appSecret:             cfg.AppSecret,
		bootstrapRefreshToken: strings.TrimSpace(cfg.ExternalImageOAuth.BootstrapRefreshToken),
		store:                 store,
		client:                client,
		tokenEndpoint:         tokenEndpoint,
		now:                   now,
	}
	if _, err := provider.AccessToken(ctx); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *ExternalImageTokenProvider) AccessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token.AccessToken != "" && p.token.AccessExpiresAt.After(p.now().Add(oauthRefreshSkew)) {
		return p.token.AccessToken, nil
	}
	if p.token.RefreshToken == "" {
		record, err := p.store.Load(ctx)
		switch {
		case err == nil:
			p.token = record
		case errors.Is(err, sql.ErrNoRows):
			if p.bootstrapRefreshToken == "" {
				return "", errors.New("no stored external-image OAuth token; run feishu-authorize on an internal group member's computer")
			}
			p.token.RefreshToken = p.bootstrapRefreshToken
		default:
			return "", fmt.Errorf("load external-image OAuth token: %w", err)
		}
		if p.token.AccessToken != "" && p.token.AccessExpiresAt.After(p.now().Add(oauthRefreshSkew)) {
			return p.token.AccessToken, nil
		}
	}

	refreshed, err := exchangeFeishuOAuthToken(ctx, p.client, p.tokenEndpoint, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     p.appID,
		"client_secret": p.appSecret,
		"refresh_token": p.token.RefreshToken,
	}, p.now)
	if err != nil {
		return "", err
	}
	if err := p.store.Save(ctx, refreshed); err != nil {
		return "", err
	}
	p.token = refreshed
	return refreshed.AccessToken, nil
}

// ExternalImageOAuthScopes is deliberately minimal: it lets a consenting user
// read group messages and retain that grant for token refreshes.
func ExternalImageOAuthScopes() []string {
	return []string{"im:message:readonly", "im:message.group_msg:get_as_user", "offline_access"}
}

// ExchangeExternalImageAuthorizationCode exchanges a one-time OAuth code from
// the local `feishu-authorize` flow. Only the refresh token is returned so the
// command can bootstrap production without printing the access token.
func ExchangeExternalImageAuthorizationCode(ctx context.Context, appID, appSecret, redirectURL, code string) (string, error) {
	record, err := exchangeFeishuOAuthToken(ctx, feishuOAuthHTTPClient, feishuOAuthTokenEndpoint, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     appID,
		"client_secret": appSecret,
		"code":          code,
		"redirect_uri":  redirectURL,
	}, time.Now)
	if err != nil {
		return "", err
	}
	return record.RefreshToken, nil
}

func exchangeFeishuOAuthToken(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	payload map[string]string,
	now func() time.Time,
) (oauthToken, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return oauthToken{}, fmt.Errorf("encode OAuth request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return oauthToken{}, fmt.Errorf("create OAuth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		return oauthToken{}, fmt.Errorf("request Feishu OAuth token: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return oauthToken{}, fmt.Errorf("read Feishu OAuth response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return oauthToken{}, fmt.Errorf("Feishu OAuth HTTP status %d", resp.StatusCode)
	}
	var response struct {
		Code                  int    `json:"code"`
		Msg                   string `json:"msg"`
		AccessToken           string `json:"access_token"`
		ExpiresIn             int    `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
		RefreshExpiresIn      int    `json:"refresh_expires_in"`
		Scope                 string `json:"scope"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return oauthToken{}, fmt.Errorf("decode Feishu OAuth response: %w", err)
	}
	if response.Code != 0 {
		return oauthToken{}, fmt.Errorf("Feishu OAuth code=%d message=%s", response.Code, response.Msg)
	}
	if response.AccessToken == "" || response.RefreshToken == "" || response.ExpiresIn <= 0 {
		return oauthToken{}, errors.New("Feishu OAuth response is missing a renewable user token")
	}
	if err := validateExternalImageOAuthScopes(response.Scope); err != nil {
		return oauthToken{}, err
	}
	record := oauthToken{
		AccessToken:     response.AccessToken,
		RefreshToken:    response.RefreshToken,
		AccessExpiresAt: now().Add(time.Duration(response.ExpiresIn) * time.Second),
	}
	refreshLifetime := response.RefreshTokenExpiresIn
	if refreshLifetime == 0 {
		refreshLifetime = response.RefreshExpiresIn
	}
	if refreshLifetime > 0 {
		record.RefreshTokenExpiresAt = now().Add(time.Duration(refreshLifetime) * time.Second)
	}
	return record, nil
}

func validateExternalImageOAuthScopes(granted string) error {
	actual := make(map[string]struct{})
	for _, scope := range strings.Fields(granted) {
		actual[scope] = struct{}{}
	}
	for _, required := range ExternalImageOAuthScopes() {
		if _, ok := actual[required]; !ok {
			return fmt.Errorf("Feishu OAuth did not grant required scope %q; enable it in the app, publish the app version, then authorize again", required)
		}
	}
	return nil
}
