package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type BitBrowserClient struct {
	baseURL string
	client  *http.Client
}

type BitBrowserOpenResult struct {
	BrowserID string `json:"browser_id"`
	HTTP      string `json:"http"`
	WS        string `json:"ws"`
}

func NewBitBrowserClient(baseURL string) *BitBrowserClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:54345"
	}
	return &BitBrowserClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *BitBrowserClient) OpenOrCreate(ctx context.Context, browserID, loginURL string) (BitBrowserOpenResult, error) {
	browserID = strings.TrimSpace(browserID)
	if browserID == "" {
		id, err := c.createProfile(ctx, loginURL)
		if err != nil {
			return BitBrowserOpenResult{}, err
		}
		browserID = id
	}
	result, err := c.openProfile(ctx, browserID)
	if err != nil {
		return BitBrowserOpenResult{}, err
	}
	result.BrowserID = browserID
	return result, nil
}

func (c *BitBrowserClient) createProfile(ctx context.Context, loginURL string) (string, error) {
	payload := map[string]any{
		"proxyMethod":        1,
		"name":               "iCloud Privacy Mail 登录态",
		"proxyType":          "noproxy",
		"url":                strings.TrimSpace(loginURL),
		"port":               "",
		"host":               "",
		"platform":           "icloud",
		"remark":             "manual login and save session",
		"browserFingerPrint": map[string]any{},
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			ID string `json:"id"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := c.post(ctx, "/browser/update", payload, &resp); err != nil {
		return "", err
	}
	if !resp.Success || strings.TrimSpace(resp.Data.ID) == "" {
		return "", fmt.Errorf("bitbrowser create failed: %s", resp.Msg)
	}
	return resp.Data.ID, nil
}

func (c *BitBrowserClient) openProfile(ctx context.Context, browserID string) (BitBrowserOpenResult, error) {
	payload := map[string]any{"id": browserID, "queue": true}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			HTTP string `json:"http"`
			WS   string `json:"ws"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := c.post(ctx, "/browser/open", payload, &resp); err != nil {
		return BitBrowserOpenResult{}, err
	}
	if !resp.Success || strings.TrimSpace(resp.Data.HTTP) == "" {
		return BitBrowserOpenResult{}, fmt.Errorf("bitbrowser open failed: %s", resp.Msg)
	}
	return BitBrowserOpenResult{HTTP: resp.Data.HTTP, WS: resp.Data.WS}, nil
}

func (c *BitBrowserClient) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bitbrowser HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return err
	}
	return nil
}
