// 飞书网页登录 OAuth 客户端：换取用户身份并返回 open_id，不保存用户令牌。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthConfig 是网页登录所需的飞书应用凭据。
type OAuthConfig struct {
	// AppID 飞书应用 App ID。
	AppID string
	// AppSecret 飞书应用 App Secret，仅保存在内存中。
	AppSecret string
	// BaseURL 飞书开放平台地址，测试时可替换。
	BaseURL string
	// HTTPTimeout 单次请求超时。
	HTTPTimeout time.Duration
}

// OAuthClient 执行飞书网页登录授权码交换与用户信息查询。
type OAuthClient struct {
	appID     string
	appSecret string
	baseURL   string
	http      *http.Client
}

// NewOAuthClient 构造网页登录客户端；缺少凭据时拒绝创建。
func NewOAuthClient(config OAuthConfig) (*OAuthClient, error) {
	if strings.TrimSpace(config.AppID) == "" || strings.TrimSpace(config.AppSecret) == "" {
		return nil, errors.New("feishu oauth app id and secret are required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://open.feishu.cn"
	}
	timeout := config.HTTPTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &OAuthClient{
		appID: config.AppID, appSecret: config.AppSecret, baseURL: baseURL,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// AuthorizeURL 生成飞书授权页地址，state 必须由服务端生成并校验。
func (c *OAuthClient) AuthorizeURL(redirectURI, state string) string {
	values := url.Values{}
	values.Set("app_id", c.appID)
	values.Set("redirect_uri", redirectURI)
	values.Set("state", state)
	return c.baseURL + "/open-apis/authen/v1/authorize?" + values.Encode()
}

// Exchange 用授权码换取用户 open_id、tenant_key 与显示名。
func (c *OAuthClient) Exchange(ctx context.Context, code, redirectURI string) (openID, tenantKey, name string, err error) {
	if strings.TrimSpace(code) == "" {
		return "", "", "", errors.New("feishu authorization code is required")
	}
	var tokenResponse struct {
		Code        int    `json:"code"`
		Msg         string `json:"msg"`
		AccessToken string `json:"access_token"`
		Data        *struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	payload := map[string]string{
		"grant_type": "authorization_code", "client_id": c.appID,
		"client_secret": c.appSecret, "code": code, "redirect_uri": redirectURI,
	}
	if err := c.post(ctx, "/open-apis/authen/v2/oauth/token", payload, "", &tokenResponse); err != nil {
		return "", "", "", err
	}
	if tokenResponse.Code != 0 {
		return "", "", "", errors.New("feishu oauth token exchange rejected")
	}
	accessToken := tokenResponse.AccessToken
	if accessToken == "" && tokenResponse.Data != nil {
		accessToken = tokenResponse.Data.AccessToken
	}
	if accessToken == "" {
		return "", "", "", errors.New("feishu oauth token exchange returned no access token")
	}

	var userResponse struct {
		Code int `json:"code"`
		Data struct {
			OpenID    string `json:"open_id"`
			TenantKey string `json:"tenant_key"`
			Name      string `json:"name"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/open-apis/authen/v1/user_info", accessToken, &userResponse); err != nil {
		return "", "", "", err
	}
	if userResponse.Code != 0 || strings.TrimSpace(userResponse.Data.OpenID) == "" {
		return "", "", "", errors.New("feishu oauth user info returned no open id")
	}
	return userResponse.Data.OpenID, userResponse.Data.TenantKey, userResponse.Data.Name, nil
}

// post 执行一次飞书 POST 请求。
func (c *OAuthClient) post(ctx context.Context, path string, payload any, accessToken string, out any) error {
	return c.do(ctx, http.MethodPost, path, payload, accessToken, out)
}

// get 执行一次飞书 GET 请求。
func (c *OAuthClient) get(ctx context.Context, path, accessToken string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, accessToken, out)
}

// do 执行一次飞书请求并限制响应体大小；错误中不保留上游原文。
func (c *OAuthClient) do(ctx context.Context, method, path string, payload any, accessToken string, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode feishu oauth request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create feishu oauth request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Accept", "application/json")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return errors.New("call feishu oauth endpoint failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return errors.New("read feishu oauth response failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("feishu oauth request failed: status=%d", response.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("decode feishu oauth response failed")
	}
	return nil
}
