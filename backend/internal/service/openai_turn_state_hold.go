package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 保留历史 model_rate_limits 中 turn-state 暂停记录的读取、恢复和错误分类。
// 真实请求不再因缺票设置暂停；这里不包含旧的注入点拦截写入逻辑。
const (
	// openAITurnStateHoldLimitReason 是 model_rate_limits 条目的 reason，猎手据此认出自己停的。
	openAITurnStateHoldLimitReason = OpenAITurnStateHoldSelectionReason
	// ctxKeyTurnStateHold 记本次请求因缺票被拦下的模型；出站构造完请求头后据此换号。
	ctxKeyTurnStateHold = "openai_turn_state_hold"
)

// OpenAITurnStateHoldReason 是换号错误的原因码，handler 据此给客户端回 503 与说明。
const OpenAITurnStateHoldReason = GatewayFailureReason("openai_turn_state_hold")

// OpenAITurnStateHoldSelectionReason 是调度过滤点给降智暂停记的 reason，会出现在空池错误的
// summary 里（"pool=1, filtered: turn_state_hold=1"）。导出是为了让 handler 的正则由它拼出来：
// 那边靠字符串认这个计数，两边各抄一份的话，改名时两侧测试都绿而生产静默掉回 429。
const OpenAITurnStateHoldSelectionReason = "turn_state_hold"

// openAITurnStateHoldEnabled 报告该模型缺票时要不要停调度：猎手开着、管这个模型、开了暂停。
func (s *OpenAIGatewayService) openAITurnStateHoldEnabled(a *Account, model string) bool {
	if !s.openAITurnStateHuntedModel(a, model) {
		return false
	}
	cfg, _ := readOpenAITurnStateHunterConfig(a)
	return cfg.HoldWhenDegraded
}

// openAITurnStateHoldResetAt 读 model_rate_limits 里本功能写的那条：不是本功能写的返回 false。
// 类型断言与 modelRateLimitResetAt 同一套（JSONB 读回来的是 map[string]any）。
func openAITurnStateHoldResetAt(a *Account, model string) (time.Time, bool) {
	if a == nil || a.Extra == nil || model == "" {
		return time.Time{}, false
	}
	limits, ok := a.Extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	entry, ok := limits[model].(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	if reason, _ := entry["reason"].(string); reason != openAITurnStateHoldLimitReason {
		return time.Time{}, false
	}
	raw, _ := entry["rate_limit_reset_at"].(string)
	resetAt, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return resetAt, true
}

// openAITurnStateModelHeld 报告该模型此刻是否因缺票被停着。
func openAITurnStateModelHeld(a *Account, model string, now time.Time) bool {
	resetAt, ok := openAITurnStateHoldResetAt(a, strings.TrimSpace(model))
	return ok && now.Before(resetAt)
}

// openAITurnStateHeldModels 列出此刻被停着的模型，按名排序让猎手轮询顺序稳定。
func openAITurnStateHeldModels(a *Account, now time.Time) []string {
	if a == nil || a.Extra == nil {
		return nil
	}
	limits, ok := a.Extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for model := range limits {
		if openAITurnStateModelHeld(a, model, now) {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}

// openAITurnStateHoldError 把注入点的拦截变成换号错误：本账号排除、下一个账号继续；
// 全池耗尽时 handler 按 OpenAITurnStateHoldReason 回 503 与 ClientMessage。
func openAITurnStateHoldError(c *gin.Context) error {
	if c == nil {
		return nil
	}
	model := strings.TrimSpace(c.GetString(ctxKeyTurnStateHold))
	if model == "" {
		return nil
	}
	return &UpstreamFailoverError{
		StatusCode:       http.StatusServiceUnavailable,
		Reason:           OpenAITurnStateHoldReason,
		ClientStatusCode: http.StatusServiceUnavailable,
		ClientMessage:    fmt.Sprintf("account has no healthy x-codex-turn-state for %s and is paused until the hunter finds one", model),
	}
}

// syncHold 每 tick 维护暂停：开关关了或票到手就放回；仍缺票就让它自然到期（不续期，下一条
// 请求会再拉起）。放回前重读一次（一次调用最多读一次）：手里的账号可能是 tick 开头的快照，
// 期间条目可能已被清掉或换成别的原因（管理员清限流后真实请求撞了 spark 429），那时不该再写。
// 轮次中新写的暂停要靠调用方先重读账号再传进来（见 huntStep 命中分支），这里的重读只挡「别写」。
func (s *OpenAITurnStateHunterService) syncHold(ctx context.Context, account *Account, now time.Time) {
	var pool []openAITurnStateCandidate
	poolLoaded := false
	var latest *Account
	for _, model := range openAITurnStateHeldModels(account, now) {
		release := ""
		switch {
		case account.Status != StatusActive || !account.IsOpenAITurnStateAutoEnabled() || !s.gateway.openAITurnStateHoldEnabled(account, model):
			release = "hold disabled"
		default:
			if !poolLoaded {
				pool, poolLoaded = s.gateway.loadOpenAITurnStatePoolFresh(ctx, account), true
			}
			if _, _, ok := pickOpenAITurnStateCandidate(pool, model, account.openAITurnStateStaleAfter(), now); ok {
				release = "ticket pooled"
			}
		}
		if release == "" {
			continue
		}
		if latest == nil {
			l, err := s.accountRepo.GetByID(ctx, account.ID)
			if err != nil || l == nil {
				slog.Warn("openai_turn_state_hold_release_failed", "account_id", account.ID, "model", model, "error", err)
				return
			}
			latest = l
		}
		if !openAITurnStateModelHeld(latest, model, now) {
			continue // 已经被别的路径清掉或换了原因
		}
		if err := s.releaseHold(ctx, account, model, now); err != nil {
			slog.Warn("openai_turn_state_hold_release_failed", "account_id", account.ID, "model", model, "error", err)
			continue
		}
		slog.Info("openai_turn_state_hold_released", "account_id", account.ID, "model", model, "reason", release)
	}
}

// releaseHold 放回：把该模型的暂停写成已到期。仓储没有按 scope 删除的方法，而 SetModelRateLimit
// 是同一条原子 UPDATE + 调度快照同步；留下的过期条目对调度无效，人工「恢复调度」会连带清掉。
func (s *OpenAITurnStateHunterService) releaseHold(ctx context.Context, account *Account, model string, now time.Time) error {
	expired := now.Add(-time.Second)
	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, model, expired, openAITurnStateHoldLimitReason); err != nil {
		return err
	}
	setAccountModelRateLimitSnapshot(account, model, expired, openAITurnStateHoldLimitReason, now)
	return nil
}
