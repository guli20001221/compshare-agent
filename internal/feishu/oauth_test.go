package feishu

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/require"
)

type memoryOAuthTokenStore struct {
	record oauthToken
	err    error
	saved  []oauthToken
}

func (s *memoryOAuthTokenStore) Load(context.Context) (oauthToken, error) {
	if s.err != nil {
		return oauthToken{}, s.err
	}
	return s.record, nil
}

func (s *memoryOAuthTokenStore) Save(_ context.Context, record oauthToken) error {
	s.record = record
	s.saved = append(s.saved, record)
	return nil
}

func TestTokenCipherRoundTripAndSecretRotationInvalidatesCiphertext(t *testing.T) {
	cipher, err := newTokenCipher("cli_test", "app-secret")
	require.NoError(t, err)
	sealed, err := cipher.seal("refresh-token")
	require.NoError(t, err)
	require.NotContains(t, sealed, "refresh-token")
	opened, err := cipher.open(sealed)
	require.NoError(t, err)
	require.Equal(t, "refresh-token", opened)

	rotated, err := newTokenCipher("cli_test", "rotated-secret")
	require.NoError(t, err)
	_, err = rotated.open(sealed)
	require.ErrorContains(t, err, "re-authorize")
}

func TestExternalImageTokenProviderBootstrapsAndPersistsRotatedToken(t *testing.T) {
	now := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	store := &memoryOAuthTokenStore{err: sql.ErrNoRows}
	var request map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		require.Equal(t, http.MethodPost, req.Method)
		require.NoError(t, json.NewDecoder(req.Body).Decode(&request))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"code":0,"access_token":"access-1","expires_in":7200,"refresh_token":"refresh-2","refresh_token_expires_in":86400,"scope":"im:message:readonly im:message.group_msg:get_as_user offline_access"}`))
	}))
	defer server.Close()

	cfg := config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "app-secret",
		ExternalImageOAuth: config.ExternalImageOAuthConfig{
			Enabled:               true,
			BootstrapRefreshToken: "bootstrap-refresh",
		},
	}
	provider, err := newExternalImageTokenProvider(context.Background(), cfg, store, server.Client(), server.URL, func() time.Time { return now })
	require.NoError(t, err)
	accessToken, err := provider.AccessToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-1", accessToken)
	require.Equal(t, "refresh_token", request["grant_type"])
	require.Equal(t, "bootstrap-refresh", request["refresh_token"])
	require.Len(t, store.saved, 1)
	require.Equal(t, "refresh-2", store.saved[0].RefreshToken)
	require.Equal(t, now.Add(2*time.Hour), store.saved[0].AccessExpiresAt)
}

func TestExternalImageTokenProviderRequiresBootstrapOrStoredToken(t *testing.T) {
	cfg := config.FeishuConfig{
		AppID: "cli_test", AppSecret: "app-secret",
		ExternalImageOAuth: config.ExternalImageOAuthConfig{Enabled: true},
	}
	_, err := newExternalImageTokenProvider(context.Background(), cfg, &memoryOAuthTokenStore{err: sql.ErrNoRows}, http.DefaultClient, "http://unused", time.Now)
	require.ErrorContains(t, err, "run feishu-authorize")
}

func TestExternalImageOAuthScopesStayMinimal(t *testing.T) {
	scopes := strings.Join(ExternalImageOAuthScopes(), " ")
	require.Equal(t, "im:message:readonly im:message.group_msg:get_as_user offline_access", scopes)
}

func TestValidateExternalImageOAuthScopesRejectsPartialGrant(t *testing.T) {
	err := validateExternalImageOAuthScopes("im:message:readonly offline_access")
	require.ErrorContains(t, err, "im:message.group_msg:get_as_user")
}
