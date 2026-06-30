//go:build liveapple

package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveAppleAccountManageLoginAndSave(t *testing.T) {
	appleID := strings.TrimSpace(os.Getenv("IPM_LIVE_APPLE_ID"))
	password := os.Getenv("IPM_LIVE_PASSWORD")
	if appleID == "" || strings.TrimSpace(password) == "" {
		t.Skip("set IPM_LIVE_APPLE_ID and IPM_LIVE_PASSWORD to run live Apple Account protocol login")
	}

	cfg, err := LoadConfig("config.json")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	store, err := NewFileStore(cfg.DataPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ownerID := liveOwnerID(store)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pendingStore := newAppleAuthPendingStore()
	result, err := NewAppleAuthClient().StartAppleAccountManageLogin(ctx, appleID, password, pendingStore)
	if err != nil {
		t.Fatalf("start Apple Account manage login: %v", err)
	}

	session := result.Session
	if result.Needs2FA {
		fmt.Fprintf(os.Stderr, "LIVE_APPLE_ACCOUNT_NEEDS_2FA apple_id=%s message=%s\n", result.AppleID, result.Message)
		code := strings.TrimSpace(os.Getenv("IPM_LIVE_2FA_CODE"))
		if code == "" {
			code = waitForLive2FACode(ctx, t)
		}
		pending, ok := pendingStore.get(result.PendingID)
		if !ok {
			t.Fatalf("pending Apple Account login expired")
		}
		phoneNumber := bytes.TrimSpace([]byte(os.Getenv("IPM_LIVE_PHONE_NUMBER_JSON")))
		session, err = NewAppleAuthClient().SubmitAppleAccountManage2FA(ctx, pending, code, phoneNumber)
		if err != nil {
			t.Fatalf("submit Apple Account manage 2FA: %v", err)
		}
	}

	if err := store.SaveICloudSessionForOwner(ownerID, session); err != nil {
		t.Fatalf("save Apple Account manage session: %v", err)
	}
	saved, ok := store.ICloudSessionForOwner(ownerID)
	if !ok {
		t.Fatalf("saved session not found")
	}
	state, ok := appleAccountLoginState(saved)
	if !ok {
		t.Fatalf("saved session missing Apple Account login state")
	}
	fmt.Fprintf(os.Stderr, "LIVE_APPLE_ACCOUNT_SAVED owner_id=%s apple_id=%s has_api_key=%t has_scnt=%t login_states=%d\n",
		ownerID,
		maskAppleID(session.AppleID),
		strings.TrimSpace(state.APIKey) != "",
		strings.TrimSpace(state.Scnt) != "",
		len(saved.LoginStates),
	)
}

func TestLiveAppleAccountCreateMailboxAndSave(t *testing.T) {
	if strings.TrimSpace(os.Getenv("IPM_LIVE_CREATE_MAILBOX")) != "1" {
		t.Skip("set IPM_LIVE_CREATE_MAILBOX=1 to create one real Apple Account private email")
	}
	cfg, err := LoadConfig("config.json")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	store, err := NewFileStore(cfg.DataPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ownerID := liveOwnerID(store)
	if ownerID == "" {
		t.Fatalf("set IPM_LIVE_OWNER_ID or keep exactly one local user before live create")
	}
	session, ok := firstLiveAppleAccountSession(store, ownerID)
	if !ok {
		t.Fatalf("saved Apple Account manage state not found; run TestLiveAppleAccountManageLoginAndSave first")
	}

	label := strings.TrimSpace(os.Getenv("IPM_LIVE_CREATE_LABEL"))
	if label == "" {
		label = "LIVE-" + time.Now().Format("0102-150405")
	}
	note := strings.TrimSpace(os.Getenv("IPM_LIVE_CREATE_NOTE"))
	if note == "" {
		note = "live Apple Account private email create test"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	remote, updatedSession, err := NewICloudClient().CreatePrivacyMailboxWithAppleAccount(ctx, session, cfg.AppleAccountAPIKey, label, note)
	if err != nil {
		t.Fatalf("create Apple Account private email: %v", err)
	}
	if remote.Origin != "APPLE_ACCOUNT" {
		t.Fatalf("remote origin = %q, want APPLE_ACCOUNT", remote.Origin)
	}
	if strings.TrimSpace(remote.Email) == "" {
		t.Fatalf("created mailbox email is empty: %+v", remote)
	}
	if err := store.SaveICloudSessionForOwner(ownerID, updatedSession); err != nil {
		t.Fatalf("save updated Apple Account state: %v", err)
	}
	mailbox, err := store.AddMailboxForOwner(ownerID, updatedSession.AccountID, remote.Label, remote.Email)
	if err != nil {
		t.Fatalf("save created mailbox record: %v", err)
	}
	if strings.TrimSpace(mailbox.AccountID) == "" || mailbox.AccountID != updatedSession.AccountID {
		t.Fatalf("mailbox account_id = %q, want %q", mailbox.AccountID, updatedSession.AccountID)
	}
	fmt.Fprintf(os.Stderr, "LIVE_APPLE_ACCOUNT_MAILBOX_CREATED owner_id=%s account_id=%s email=%s origin=%s mailbox_id=%s\n",
		ownerID,
		updatedSession.AccountID,
		maskAppleID(remote.Email),
		remote.Origin,
		mailbox.ID,
	)
}

func liveOwnerID(store *FileStore) string {
	ownerID := strings.TrimSpace(os.Getenv("IPM_LIVE_OWNER_ID"))
	if ownerID != "" || store == nil {
		return ownerID
	}
	users := store.Users()
	if len(users) == 1 {
		return users[0].ID
	}
	return ""
}

func firstLiveAppleAccountSession(store *FileStore, ownerID string) (ICloudSession, bool) {
	for _, session := range store.ICloudSessionsForOwner(ownerID) {
		if appleAccountManageReady(session) {
			return session, true
		}
	}
	return ICloudSession{}, false
}

func waitForLive2FACode(ctx context.Context, t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("IPM_LIVE_2FA_CODE_FILE"))
	if path == "" {
		path = filepath.Join("data", "live_2fa_code.txt")
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	_ = os.Remove(path)
	fmt.Fprintf(os.Stderr, "LIVE_APPLE_ACCOUNT_WAITING_2FA_FILE path=%s\n", path)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("wait 2FA code: %v", ctx.Err())
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			code := strings.TrimSpace(string(data))
			_ = os.Remove(path)
			if len(code) == 6 {
				return code
			}
			t.Fatalf("2FA code file must contain 6 digits")
		}
	}
}
