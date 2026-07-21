package service

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// anthropicSetupTokenVPSSyncTrigger 在本机 Anthropic setup-token 刷新成功后异步触发推 VPS 脚本。
// 仅凭证权威节点启用；脚本自身 lock 与 debounce 防止并发/连刷打爆 SSH。
//
// 防抖语义：窗口内的多次 Notify 不会连跑脚本，但会在窗口结束后再跑一次（trailing），
// 避免“先后台刷新触发 → 管理端再刷”时丢掉最新凭证。
type anthropicSetupTokenVPSSyncTrigger struct {
	enabled  bool
	command  string
	debounce time.Duration

	mu            sync.Mutex
	lastStart     time.Time // 最近一次真正启动 run 的时间
	pending       bool      // 窗口内是否有待补跑
	pendingAcctID int64
	timer         *time.Timer
	running       bool // 是否有 run 在执行（含脚本）
}

func newAnthropicSetupTokenVPSSyncTrigger(enabled bool, command string, debounceSeconds int) *anthropicSetupTokenVPSSyncTrigger {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		cmd = "deploy/run_vps_anthropic_setup_token_sync_launchd.sh"
	}
	debounce := time.Duration(debounceSeconds) * time.Second
	if debounce <= 0 {
		debounce = 30 * time.Second
	}
	return &anthropicSetupTokenVPSSyncTrigger{
		enabled:  enabled,
		command:  cmd,
		debounce: debounce,
	}
}

func (t *anthropicSetupTokenVPSSyncTrigger) enabledFor(account *Account) bool {
	if t == nil || !t.enabled || account == nil {
		return false
	}
	return account.Platform == PlatformAnthropic && account.Type == AccountTypeSetupToken
}

// NotifyRefreshed 刷新成功后调用；异步、可防抖，不阻塞刷新主路径。
func (t *anthropicSetupTokenVPSSyncTrigger) NotifyRefreshed(account *Account) {
	if t == nil {
		return
	}
	if account == nil {
		t.audit("skip_nil_account", 0, "")
		return
	}
	if !t.enabled {
		t.audit("skip_disabled", account.ID, fmt.Sprintf("platform=%s type=%s", account.Platform, account.Type))
		return
	}
	if account.Platform != PlatformAnthropic || account.Type != AccountTypeSetupToken {
		t.audit("skip_not_setup_token", account.ID, fmt.Sprintf("platform=%s type=%s", account.Platform, account.Type))
		return
	}

	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	// 无进行中的 run，且距上次启动已过 debounce：立刻跑。
	if !t.running && (t.lastStart.IsZero() || now.Sub(t.lastStart) >= t.debounce) {
		t.lastStart = now
		t.pending = false
		t.pendingAcctID = 0
		if t.timer != nil {
			t.timer.Stop()
			t.timer = nil
		}
		t.running = true
		command := t.command
		accountID := account.ID
		t.audit("scheduled_immediate", accountID, command)
		go t.run(accountID, command)
		return
	}

	// 窗口内或脚本仍在跑：标记 trailing 补跑，保证最终推到最新凭证。
	t.pending = true
	t.pendingAcctID = account.ID
	wait := t.debounce
	if !t.lastStart.IsZero() {
		if elapsed := now.Sub(t.lastStart); elapsed < t.debounce {
			wait = t.debounce - elapsed
		} else {
			wait = 0
		}
	}
	// 若仍有 run 在执行，至少等一小段再尝试，避免与当前脚本抢 lock。
	if t.running && wait < 2*time.Second {
		wait = 2 * time.Second
	}

	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	accountID := account.ID
	t.audit("scheduled_trailing", accountID, fmt.Sprintf("wait=%s running=%v", wait, t.running))
	slog.Info("token_refresh.anthropic_setup_token_vps_sync_trailing",
		"account_id", accountID,
		"wait", wait.String(),
		"running", t.running,
	)
	t.timer = time.AfterFunc(wait, func() {
		t.firePending()
	})
}

func (t *anthropicSetupTokenVPSSyncTrigger) firePending() {
	t.mu.Lock()
	if !t.pending {
		t.timer = nil
		t.mu.Unlock()
		return
	}
	if t.running {
		// 仍在跑：再等一会
		t.timer = time.AfterFunc(2*time.Second, func() {
			t.firePending()
		})
		t.mu.Unlock()
		return
	}
	accountID := t.pendingAcctID
	command := t.command
	t.pending = false
	t.pendingAcctID = 0
	t.timer = nil
	t.lastStart = time.Now()
	t.running = true
	t.audit("scheduled_trailing_fire", accountID, command)
	t.mu.Unlock()
	go t.run(accountID, command)
}

func (t *anthropicSetupTokenVPSSyncTrigger) run(accountID int64, command string) {
	defer func() {
		t.mu.Lock()
		t.running = false
		needAgain := t.pending
		t.mu.Unlock()
		if needAgain {
			// 跑完后若仍有 pending，尽快补一次
			t.firePending()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	absCmd := command
	if !filepath.IsAbs(command) {
		if wd, err := os.Getwd(); err == nil {
			candidate := filepath.Join(wd, command)
			if _, err := os.Stat(candidate); err == nil {
				absCmd = candidate
			}
		}
	}

	// 双写 slog + log.Printf + 独立审计文件，避免 stdlog bridge / 采样导致“同步了却看不见日志”。
	slog.Info("token_refresh.anthropic_setup_token_vps_sync_triggered",
		"account_id", accountID,
		"command", absCmd,
	)
	log.Printf("[token_refresh] anthropic_setup_token_vps_sync_triggered account_id=%d command=%s", accountID, absCmd)
	t.audit("triggered", accountID, absCmd)

	// 用 shell 包装：兼容 launchd 包装脚本（#!/bin/zsh）与 bash 同步脚本。
	cmd := exec.CommandContext(ctx, "/bin/zsh", absCmd)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		// 脚本 lock 冲突时 exit 0 且打印 skipping，不会进这里。
		out := strings.TrimSpace(string(output))
		slog.Warn("token_refresh.anthropic_setup_token_vps_sync_failed",
			"account_id", accountID,
			"command", absCmd,
			"error", err,
			"output", out,
		)
		log.Printf("[token_refresh] anthropic_setup_token_vps_sync_failed account_id=%d command=%s err=%v output_bytes=%d", accountID, absCmd, err, len(output))
		t.audit("failed", accountID, fmt.Sprintf("err=%v output_bytes=%d", err, len(output)))
		return
	}
	// 成功时只记摘要，避免把 token/凭证写进日志。
	slog.Info("token_refresh.anthropic_setup_token_vps_sync_finished",
		"account_id", accountID,
		"output_bytes", len(output),
	)
	log.Printf("[token_refresh] anthropic_setup_token_vps_sync_finished account_id=%d output_bytes=%d", accountID, len(output))
	t.audit("finished", accountID, fmt.Sprintf("output_bytes=%d", len(output)))
}

// audit 追加写固定审计文件，路径相对进程 cwd（host 的 WorkingDirectory 为仓库根）。
func (t *anthropicSetupTokenVPSSyncTrigger) audit(event string, accountID int64, detail string) {
	line := fmt.Sprintf("%s event=%s account_id=%d %s\n", time.Now().Format(time.RFC3339Nano), event, accountID, detail)
	// 尽力写；失败时仍走 slog，不因审计失败影响主路径。
	paths := []string{
		"deploy/data-host/logs/anthropic_setup_token_vps_sync.audit.log",
		filepath.Join(os.TempDir(), "sub2api_anthropic_setup_token_vps_sync.audit.log"),
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			continue
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		_, _ = f.WriteString(line)
		_ = f.Close()
		return
	}
	slog.Warn("token_refresh.anthropic_setup_token_vps_sync_audit_failed", "event", event, "account_id", accountID)
}
