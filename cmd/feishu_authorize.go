package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	feishuadapter "github.com/compshare-agent/internal/feishu"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const defaultFeishuAuthorizeTimeout = 5 * time.Minute

var (
	feishuAuthorizeTimeout time.Duration
	feishuAuthorizeEnable  bool
)

var feishuAuthorizeCmd = &cobra.Command{
	Use:   "feishu-authorize",
	Short: "在本机授权外部群截图读取",
	RunE:  runFeishuAuthorize,
}

func init() {
	feishuAuthorizeCmd.Flags().DurationVar(&feishuAuthorizeTimeout, "timeout", defaultFeishuAuthorizeTimeout, "等待飞书授权的最长时间")
	feishuAuthorizeCmd.Flags().BoolVar(&feishuAuthorizeEnable, "enable", false, "授权后同时开启 external_image_oauth")
	rootCmd.AddCommand(feishuAuthorizeCmd)
}

func runFeishuAuthorize(_ *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	redirectURL := strings.TrimSpace(cfg.Agent.Feishu.ExternalImageOAuth.RedirectURL)
	callbackURL, err := validateLoopbackRedirectURL(redirectURL)
	if err != nil {
		return err
	}
	if feishuAuthorizeTimeout <= 0 {
		return errors.New("--timeout must be positive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), feishuAuthorizeTimeout)
	defer cancel()
	code, err := receiveFeishuAuthorizationCode(ctx, cfg.Agent.Feishu.AppID, redirectURL, callbackURL)
	if err != nil {
		return err
	}
	refreshToken, err := feishuadapter.ExchangeExternalImageAuthorizationCode(
		ctx, cfg.Agent.Feishu.AppID, cfg.Agent.Feishu.AppSecret, redirectURL, code,
	)
	if err != nil {
		return fmt.Errorf("exchange Feishu authorization code: %w", err)
	}
	configFile, err := writableConfigPath()
	if err != nil {
		return err
	}
	writtenPath, err := writeFeishuOAuthBootstrapToken(configFile, refreshToken, feishuAuthorizeEnable)
	if err != nil {
		return err
	}
	if feishuAuthorizeEnable {
		fmt.Printf("飞书截图授权已写入 %s；发布前请先运行 0012 数据库迁移。\n", writtenPath)
		return nil
	}
	fmt.Printf("飞书截图授权已写入 %s；完成 0012 数据库迁移后，再将 external_image_oauth.enabled 设为 true 并发布。\n", writtenPath)
	return nil
}

func validateLoopbackRedirectURL(raw string) (*url.URL, error) {
	callbackURL, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse agent.feishu.external_image_oauth.redirect_url: %w", err)
	}
	if callbackURL.Scheme != "http" || callbackURL.Host == "" || callbackURL.Path == "" || callbackURL.RawQuery != "" || callbackURL.Fragment != "" {
		return nil, errors.New("agent.feishu.external_image_oauth.redirect_url must be a loopback HTTP callback without query or fragment")
	}
	host := strings.Trim(strings.ToLower(callbackURL.Hostname()), "[]")
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return nil, errors.New("agent.feishu.external_image_oauth.redirect_url must use localhost, 127.0.0.1, or ::1")
	}
	if callbackURL.Port() == "" {
		return nil, errors.New("agent.feishu.external_image_oauth.redirect_url must include a port")
	}
	return callbackURL, nil
}

func receiveFeishuAuthorizationCode(ctx context.Context, appID, redirectURL string, callbackURL *url.URL) (string, error) {
	state, err := newOAuthState()
	if err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp", callbackURL.Host)
	if err != nil {
		return "", fmt.Errorf("listen for Feishu OAuth callback: %w", err)
	}
	defer listener.Close()

	result := make(chan error, 1)
	code := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callbackURL.Path, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Query().Get("state") != state {
			http.Error(w, "invalid authorization callback", http.StatusBadRequest)
			select {
			case result <- errors.New("Feishu OAuth callback state did not match"):
			default:
			}
			return
		}
		if callbackErr := request.URL.Query().Get("error"); callbackErr != "" {
			http.Error(w, "authorization was declined", http.StatusForbidden)
			select {
			case result <- fmt.Errorf("Feishu OAuth authorization failed: %s", callbackErr):
			default:
			}
			return
		}
		value := request.URL.Query().Get("code")
		if value == "" {
			http.Error(w, "authorization code is missing", http.StatusBadRequest)
			select {
			case result <- errors.New("Feishu OAuth callback did not contain a code"):
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<p>飞书授权完成，可以关闭此页面并返回终端。</p>"))
		select {
		case code <- value:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serverErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authorizationURL, err := buildFeishuAuthorizationURL(appID, redirectURL, state)
	if err != nil {
		return "", err
	}
	fmt.Printf("请在浏览器中完成授权：\n%s\n", authorizationURL)
	if err := openBrowser(authorizationURL); err != nil {
		fmt.Println("未能自动打开浏览器，请复制上面的地址打开。")
	}

	select {
	case value := <-code:
		return value, nil
	case err := <-result:
		return "", err
	case err := <-serverErr:
		return "", fmt.Errorf("serve Feishu OAuth callback: %w", err)
	case <-ctx.Done():
		return "", fmt.Errorf("wait for Feishu OAuth authorization: %w", ctx.Err())
	}
}

func buildFeishuAuthorizationURL(appID, redirectURL, state string) (string, error) {
	if strings.TrimSpace(appID) == "" {
		return "", errors.New("agent.feishu.app_id is required")
	}
	values := url.Values{
		"client_id":     {appID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURL},
		"scope":         {strings.Join(feishuadapter.ExternalImageOAuthScopes(), " ")},
		"state":         {state},
		"prompt":        {"consent"},
	}
	return "https://accounts.feishu.cn/open-apis/authen/v1/authorize?" + values.Encode(), nil
}

func newOAuthState() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func openBrowser(rawURL string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	case "darwin":
		return exec.Command("open", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}

func writableConfigPath() (string, error) {
	path := configPath
	if configPath == defaultConfigPath {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			path = filepath.Join("..", defaultConfigPath)
		}
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve writable config path: %w", err)
	}
	return baseConfigPath(absolutePath, map[string]struct{}{})
}

func baseConfigPath(path string, seen map[string]struct{}) (string, error) {
	if _, ok := seen[path]; ok {
		return "", fmt.Errorf("config extends cycle at %s", path)
	}
	seen[path] = struct{}{}
	defer delete(seen, path)

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config for OAuth bootstrap: %w", err)
	}
	var raw struct {
		Extends string `yaml:"extends"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parse config for OAuth bootstrap: %w", err)
	}
	if strings.TrimSpace(raw.Extends) == "" {
		return path, nil
	}
	if filepath.IsAbs(raw.Extends) {
		return "", errors.New("config extends must be relative")
	}
	next, err := filepath.Abs(filepath.Join(filepath.Dir(path), raw.Extends))
	if err != nil {
		return "", fmt.Errorf("resolve base config: %w", err)
	}
	return baseConfigPath(next, seen)
}

func writeFeishuOAuthBootstrapToken(configFile, refreshToken string, enable bool) (string, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return "", errors.New("refusing to write an empty OAuth refresh token")
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		return "", fmt.Errorf("read OAuth bootstrap config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("parse OAuth bootstrap config: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("OAuth bootstrap config must be a YAML mapping")
	}
	agent := ensureYAMLMapValue(document.Content[0], "agent")
	feishu := ensureYAMLMapValue(agent, "feishu")
	oauth := ensureYAMLMapValue(feishu, "external_image_oauth")
	setYAMLScalar(oauth, "bootstrap_refresh_token", refreshToken)
	if enable {
		setYAMLBool(oauth, "enabled", true)
	}

	temporary, err := os.CreateTemp(filepath.Dir(configFile), ".feishu-oauth-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create OAuth bootstrap config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("secure OAuth bootstrap config: %w", err)
	}
	encoder := yaml.NewEncoder(temporary)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		_ = encoder.Close()
		_ = temporary.Close()
		return "", fmt.Errorf("render OAuth bootstrap config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("finish OAuth bootstrap config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close OAuth bootstrap config: %w", err)
	}
	if err := os.Rename(temporaryPath, configFile); err != nil {
		return "", fmt.Errorf("replace OAuth bootstrap config: %w", err)
	}
	return configFile, nil
}

func ensureYAMLMapValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			value := mapping.Content[index+1]
			if value.Kind != yaml.MappingNode {
				value.Kind = yaml.MappingNode
				value.Tag = "!!map"
				value.Value = ""
				value.Content = nil
			}
			return value
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapping.Content = append(mapping.Content, keyNode, valueNode)
	return valueNode
}

func setYAMLScalar(mapping *yaml.Node, key, value string) {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle},
	)
}

func setYAMLBool(mapping *yaml.Node, key string, value bool) {
	encoded := "false"
	if value {
		encoded = "true"
	}
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: encoded}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: encoded},
	)
}
