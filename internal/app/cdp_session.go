package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type CDPSessionClient struct {
	httpClient *http.Client
	nextID     atomic.Int64
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type cdpMessage struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params any             `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func NewCDPSessionClient() *CDPSessionClient {
	return &CDPSessionClient{httpClient: &http.Client{Timeout: 15 * time.Second}}
}

func (c *CDPSessionClient) SaveICloudSession(ctx context.Context, cdpHTTP, defaultHost string) (ICloudSession, error) {
	target, err := c.findICloudTarget(ctx, cdpHTTP)
	if err != nil {
		return ICloudSession{}, err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return ICloudSession{}, fmt.Errorf("connect cdp: %w", err)
	}
	defer conn.Close()

	if _, err := c.call(ctx, conn, "Network.enable", map[string]any{}); err != nil {
		return ICloudSession{}, err
	}
	if _, err := c.call(ctx, conn, "Runtime.enable", map[string]any{}); err != nil {
		return ICloudSession{}, err
	}

	cookies, err := c.readCookies(ctx, conn)
	if err != nil {
		return ICloudSession{}, err
	}
	validate, err := c.validateICloud(ctx, conn, defaultHost)
	if err != nil {
		return ICloudSession{}, err
	}
	if strings.TrimSpace(validate.DSID) == "" || strings.TrimSpace(validate.PremiumMailBaseURL) == "" {
		return ICloudSession{}, errCode("icloud_not_logged_in", "未检测到有效 iCloud 登录态，请先在打开的窗口完成登录", true)
	}

	return ICloudSession{
		SavedAt:            time.Now(),
		AppleID:            validate.AppleID,
		DSID:               validate.DSID,
		ClientID:           validate.ClientID,
		ClientBuildNumber:  validate.ClientBuildNumber,
		MasteringNumber:    validate.MasteringNumber,
		PremiumMailBaseURL: strings.TrimRight(validate.PremiumMailBaseURL, "/"),
		MailGatewayBaseURL: strings.TrimRight(validate.MailGatewayBaseURL, "/"),
		MailBaseURL:        strings.TrimRight(validate.MailBaseURL, "/"),
		Host:               hostFromURL(validate.PremiumMailBaseURL, target.URL),
		IsICloudPlus:       validate.IsICloudPlus,
		CanCreateHME:       validate.CanCreateHME,
		Cookies:            cookies,
		Note:               "saved from manual browser login",
	}, nil
}

func (c *CDPSessionClient) findICloudTarget(ctx context.Context, cdpHTTP string) (cdpTarget, error) {
	base := normalizeCDPHTTP(cdpHTTP)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
	if err != nil {
		return cdpTarget{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return cdpTarget{}, fmt.Errorf("read cdp targets: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return cdpTarget{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cdpTarget{}, fmt.Errorf("cdp target HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var targets []cdpTarget
	if err := json.Unmarshal(data, &targets); err != nil {
		return cdpTarget{}, err
	}
	for _, target := range targets {
		if target.Type == "page" && target.WebSocketDebuggerURL != "" && strings.Contains(strings.ToLower(target.URL), "icloud.com") {
			return target, nil
		}
	}
	return cdpTarget{}, errCode("icloud_page_not_found", "未找到 iCloud 页面，请先打开 iCloud 登录窗口", true)
}

func (c *CDPSessionClient) readCookies(ctx context.Context, conn *websocket.Conn) ([]SessionCookie, error) {
	raw, err := c.call(ctx, conn, "Network.getAllCookies", map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Cookies []SessionCookie `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	out := make([]SessionCookie, 0, len(parsed.Cookies))
	for _, cookie := range parsed.Cookies {
		domain := strings.ToLower(cookie.Domain)
		if strings.Contains(domain, "icloud.com") || strings.Contains(domain, "apple.com") {
			if cookie.Path == "" {
				cookie.Path = "/"
			}
			out = append(out, cookie)
		}
	}
	if len(out) == 0 {
		return nil, errCode("icloud_cookie_empty", "未读取到 iCloud Cookie，请确认已登录", true)
	}
	return out, nil
}

type validateResult struct {
	AppleID            string
	DSID               string
	ClientID           string
	ClientBuildNumber  string
	MasteringNumber    string
	PremiumMailBaseURL string
	MailGatewayBaseURL string
	MailBaseURL        string
	IsICloudPlus       bool
	CanCreateHME       bool
}

func (c *CDPSessionClient) validateICloud(ctx context.Context, conn *websocket.Conn, defaultHost string) (validateResult, error) {
	host := strings.TrimSpace(defaultHost)
	if host == "" {
		host = "www.icloud.com.cn"
	}
	setupHost := "setup.icloud.com.cn"
	if strings.HasSuffix(host, "icloud.com") && !strings.HasSuffix(host, "icloud.com.cn") {
		setupHost = "setup.icloud.com"
	}
	js := fmt.Sprintf(`(async () => {
  const build = globalThis.__CW_BUILD_INFO || {};
  const uuid = () => (crypto && crypto.randomUUID) ? crypto.randomUUID() : String(Date.now()) + Math.random().toString(16).slice(2);
  const clientId = uuid();
  const buildNumber = build.buildNumber || "2618Build21";
  const masteringNumber = build.masteringNumber || buildNumber;
  const params = new URLSearchParams({
    clientBuildNumber: buildNumber,
    clientMasteringNumber: masteringNumber,
    clientId: clientId,
    requestId: uuid()
  });
  const res = await fetch("https://%s/setup/ws/1/validate?" + params.toString(), {
    method: "POST",
    credentials: "include",
    headers: {"Content-Type": "text/plain"}
  });
  const text = await res.text();
  return JSON.stringify({ok: res.ok, status: res.status, text, clientId, buildNumber, masteringNumber});
})()`, setupHost)
	raw, err := c.call(ctx, conn, "Runtime.evaluate", map[string]any{
		"expression":    js,
		"awaitPromise":  true,
		"returnByValue": true,
	})
	if err != nil {
		return validateResult{}, err
	}
	var eval struct {
		Result struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &eval); err != nil {
		return validateResult{}, err
	}
	if eval.ExceptionDetails != nil {
		return validateResult{}, errCode("icloud_validate_failed", "iCloud 登录态校验脚本执行失败", true)
	}
	var outer struct {
		OK              bool   `json:"ok"`
		Status          int    `json:"status"`
		Text            string `json:"text"`
		ClientID        string `json:"clientId"`
		BuildNumber     string `json:"buildNumber"`
		MasteringNumber string `json:"masteringNumber"`
	}
	if err := json.Unmarshal([]byte(eval.Result.Value), &outer); err != nil {
		return validateResult{}, err
	}
	if !outer.OK {
		return validateResult{}, errCode("icloud_validate_failed", fmt.Sprintf("iCloud 登录态校验失败，HTTP %d", outer.Status), true)
	}
	var account struct {
		DSInfo struct {
			DSID                            string `json:"dsid"`
			AppleID                         string `json:"appleId"`
			PrimaryEmail                    string `json:"primaryEmail"`
			IsHideMyEmailSubscriptionActive bool   `json:"isHideMyEmailSubscriptionActive"`
			IsHideMyEmailFeatureAvailable   bool   `json:"isHideMyEmailFeatureAvailable"`
		} `json:"dsInfo"`
		Webservices map[string]struct {
			URL    string `json:"url"`
			Status string `json:"status"`
		} `json:"webservices"`
	}
	if err := json.Unmarshal([]byte(outer.Text), &account); err != nil {
		return validateResult{}, errCode("icloud_validate_bad_response", "iCloud 登录态校验返回无法解析", true)
	}
	premium := account.Webservices["premiummailsettings"].URL
	mailGateway := account.Webservices["mccgateway"].URL
	mail := account.Webservices["mail"].URL
	appleID := account.DSInfo.AppleID
	if appleID == "" {
		appleID = account.DSInfo.PrimaryEmail
	}
	return validateResult{
		AppleID:            appleID,
		DSID:               account.DSInfo.DSID,
		ClientID:           outer.ClientID,
		ClientBuildNumber:  outer.BuildNumber,
		MasteringNumber:    outer.MasteringNumber,
		PremiumMailBaseURL: premium,
		MailGatewayBaseURL: mailGateway,
		MailBaseURL:        mail,
		IsICloudPlus:       account.DSInfo.IsHideMyEmailSubscriptionActive,
		CanCreateHME:       account.DSInfo.IsHideMyEmailFeatureAvailable,
	}, nil
}

func (c *CDPSessionClient) call(ctx context.Context, conn *websocket.Conn, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	if err := conn.WriteJSON(cdpMessage{ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		var msg cdpMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return nil, err
		}
		if msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("cdp %s: %s", method, msg.Error.Message)
		}
		return msg.Result, nil
	}
}

func normalizeCDPHTTP(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		value = "http://" + value
	}
	return value
}

func hostFromURL(values ...string) string {
	for _, value := range values {
		parsed, err := url.Parse(value)
		if err == nil && parsed.Host != "" {
			return parsed.Host
		}
	}
	return ""
}
