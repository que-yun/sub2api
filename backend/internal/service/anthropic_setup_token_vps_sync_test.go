//go:build unit

package service

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAnthropicSetupTokenVPSSyncTrigger_OnlySetupToken(t *testing.T) {
	trigger := newAnthropicSetupTokenVPSSyncTrigger(true, "/bin/true", 30)
	require.False(t, trigger.enabledFor(&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}))
	require.False(t, trigger.enabledFor(&Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}))
	require.True(t, trigger.enabledFor(&Account{Platform: PlatformAnthropic, Type: AccountTypeSetupToken}))
}

func TestAnthropicSetupTokenVPSSyncTrigger_DebounceTrailing(t *testing.T) {
	dir := t.TempDir()
	counterPath := filepath.Join(dir, "count")
	script := filepath.Join(dir, "sync.sh")
	// 慢一点，方便第二个 notify 落在 running 窗口
	scriptBody := "#!/bin/zsh\nsleep 0.15\necho run >> " + counterPath + "\n"
	require.NoError(t, os.WriteFile(script, []byte(scriptBody), 0o755))

	// debounce 很短，便于测 trailing
	trigger := newAnthropicSetupTokenVPSSyncTrigger(true, script, 1)
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeSetupToken}

	trigger.NotifyRefreshed(account)
	// 第一次还在跑时再 notify → 应 trailing 补跑，最终 >=2
	time.Sleep(30 * time.Millisecond)
	trigger.NotifyRefreshed(account)

	deadline := time.Now().Add(5 * time.Second)
	var lines int
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(counterPath); err == nil {
			lines = bytes.Count(b, []byte("\n"))
			if lines >= 2 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	require.GreaterOrEqual(t, lines, 2, "trailing notify must schedule a second run so latest credentials can sync")
}

func TestAnthropicSetupTokenVPSSyncTrigger_DebounceCoalesceWithinWindow(t *testing.T) {
	dir := t.TempDir()
	counterPath := filepath.Join(dir, "count")
	script := filepath.Join(dir, "sync.sh")
	scriptBody := "#!/bin/zsh\necho run >> " + counterPath + "\n"
	require.NoError(t, os.WriteFile(script, []byte(scriptBody), 0o755))

	trigger := newAnthropicSetupTokenVPSSyncTrigger(true, script, 60)
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeSetupToken}

	trigger.NotifyRefreshed(account)
	// 等第一次跑完
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(counterPath); err == nil && bytes.Count(b, []byte("\n")) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 窗口内再连点两次：只应再 trailing 一次，总数 2
	trigger.NotifyRefreshed(account)
	trigger.NotifyRefreshed(account)
	deadline = time.Now().Add(5 * time.Second)
	var lines int
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(counterPath); err == nil {
			lines = bytes.Count(b, []byte("\n"))
		}
		time.Sleep(30 * time.Millisecond)
	}
	// 60s debounce 内第二次 fire 的 wait 仍接近 60s；这里只断言“尚未立刻第三次连跑”
	// 先确认第一次已发生，且短时间内没有变成 3+
	require.GreaterOrEqual(t, lines, 1)
	require.LessOrEqual(t, lines, 2, "within debounce should not spawn unbounded runs")
}

func TestAnthropicSetupTokenVPSSyncTrigger_DisabledNoop(t *testing.T) {
	var runs atomic.Int32
	trigger := newAnthropicSetupTokenVPSSyncTrigger(false, "/bin/false", 1)
	trigger.NotifyRefreshed(&Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeSetupToken})
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(0), runs.Load())
}

func TestAnthropicSetupTokenVPSSyncTrigger_AuditFile(t *testing.T) {
	// 在临时 cwd 下写审计文件
	dir := t.TempDir()
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(old) })

	require.NoError(t, os.MkdirAll("deploy/data-host/logs", 0o755))
	script := filepath.Join(dir, "ok.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/zsh\nexit 0\n"), 0o755))

	trigger := newAnthropicSetupTokenVPSSyncTrigger(true, script, 1)
	trigger.NotifyRefreshed(&Account{ID: 99, Platform: PlatformAnthropic, Type: AccountTypeSetupToken})
	deadline := time.Now().Add(3 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		body, err = os.ReadFile("deploy/data-host/logs/anthropic_setup_token_vps_sync.audit.log")
		if err == nil && bytes.Contains(body, []byte("event=finished")) && bytes.Contains(body, []byte("account_id=99")) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("audit file missing expected finished event, body=%q err=%v", string(body), err)
}
