package billing

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

// newTestStore 打开临时库并迁移。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ledger-test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return st
}

// seedQuota 写入测试令牌与用户（初始余额 remain/quota 均为 initial），返回令牌 ID。
func seedQuota(t *testing.T, st *store.Store, initial int64) (tokenID, userID int64) {
	t.Helper()
	user := &model.User{Username: "u-test", Status: model.StatusEnabled, Quota: initial}
	if err := st.Write.Create(user).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
	tok := &model.Token{UserID: user.ID, Name: "t", Key: "k", KeyHash: "h-test",
		Status: model.StatusEnabled, Quota: initial, RemainQuota: initial}
	if err := st.Write.Create(tok).Error; err != nil {
		t.Fatalf("写入令牌失败: %v", err)
	}
	return tok.ID, user.ID
}

// reloadToken 重新读取令牌余额。
func reloadToken(t *testing.T, st *store.Store, id int64) model.Token {
	t.Helper()
	var tok model.Token
	if err := st.Read.First(&tok, id).Error; err != nil {
		t.Fatalf("读取令牌失败: %v", err)
	}
	return tok
}

func reloadUser(t *testing.T, st *store.Store, id int64) model.User {
	t.Helper()
	var u model.User
	if err := st.Read.First(&u, id).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	return u
}

// TestFreezeAndSettle 冻结 → 实结小于预扣 → 退还差额；tokens 与 users 同步。
func TestFreezeAndSettle(t *testing.T) {
	st := newTestStore(t)
	l := NewLedger(st)
	tokID, uid := seedQuota(t, st, 10_000)

	if err := l.Freeze(tokID, uid, 1_000); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != 9_000 {
		t.Fatalf("冻结后 remain 应 9000，实际 %d", tok.RemainQuota)
	}
	if u := reloadUser(t, st, uid); u.Quota != 9_000 {
		t.Fatalf("冻结后用户 quota 应 9000，实际 %d", u.Quota)
	}

	// 实结 700 → 退还 300。
	if err := l.Settle(tokID, uid, 1_000, 700); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != 9_300 {
		t.Fatalf("退款后 remain 应 9300，实际 %d", tok.RemainQuota)
	}
	if u := reloadUser(t, st, uid); u.Quota != 9_300 {
		t.Fatalf("退款后用户 quota 应 9300，实际 %d", u.Quota)
	}
}

// TestSettleOvercharge 补扣差额允许透支到负数（docs/04：负余额触发拒绝后续请求）。
func TestSettleOvercharge(t *testing.T) {
	st := newTestStore(t)
	l := NewLedger(st)
	tokID, uid := seedQuota(t, st, 100)

	if err := l.Freeze(tokID, uid, 100); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	// 实结 160 → 补扣 60 → remain = -60。
	if err := l.Settle(tokID, uid, 100, 160); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != -60 {
		t.Fatalf("补扣后 remain 应 -60，实际 %d", tok.RemainQuota)
	}
	if u := reloadUser(t, st, uid); u.Quota != -60 {
		t.Fatalf("补扣后用户 quota 应 -60，实际 %d", u.Quota)
	}
}

// TestFreezeInsufficient 余额不足拒绝冻结，无副作用。
func TestFreezeInsufficient(t *testing.T) {
	st := newTestStore(t)
	l := NewLedger(st)
	tokID, uid := seedQuota(t, st, 50)

	err := l.Freeze(tokID, uid, 51)
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("应返回 ErrInsufficientQuota，实际 %v", err)
	}
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != 50 {
		t.Fatalf("冻结失败不得扣减余额，实际 %d", tok.RemainQuota)
	}
	if u := reloadUser(t, st, uid); u.Quota != 50 {
		t.Fatalf("冻结失败不得扣减用户余额，实际 %d", u.Quota)
	}

	// 恰好相等：允许（remain_quota >= delta）。
	if err := l.Freeze(tokID, uid, 50); err != nil {
		t.Fatalf("余额恰好相等应允许冻结: %v", err)
	}
}

// TestRefundFull 全额退还。
func TestRefundFull(t *testing.T) {
	st := newTestStore(t)
	l := NewLedger(st)
	tokID, uid := seedQuota(t, st, 500)

	if err := l.Freeze(tokID, uid, 300); err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	if err := l.RefundFull(tokID, uid, 300); err != nil {
		t.Fatalf("全额退款失败: %v", err)
	}
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != 500 {
		t.Fatalf("退款后 remain 应恢复 500，实际 %d", tok.RemainQuota)
	}
	if u := reloadUser(t, st, uid); u.Quota != 500 {
		t.Fatalf("退款后用户 quota 应恢复 500，实际 %d", u.Quota)
	}
}

// TestFreezeZeroDelta delta<=0 幂等无操作（unlimited 令牌跳过路径）。
func TestFreezeZeroDelta(t *testing.T) {
	st := newTestStore(t)
	l := NewLedger(st)
	tokID, uid := seedQuota(t, st, 10)

	if err := l.Freeze(tokID, uid, 0); err != nil {
		t.Fatalf("delta=0 应直接成功: %v", err)
	}
	if err := l.Settle(tokID, uid, 0, 0); err != nil {
		t.Fatalf("零结算应幂等: %v", err)
	}
	if err := l.RefundFull(tokID, uid, 0); err != nil {
		t.Fatalf("零退款应幂等: %v", err)
	}
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != 10 {
		t.Fatalf("余额不应变化，实际 %d", tok.RemainQuota)
	}
}

// TestConcurrentFreezeNoOverdraft 并发冻结防透支（-race）：总余额 100，
// 32 个并发各冻结 10 → 恰好 10 次成功，remain 恰好归 0，绝无透支。
func TestConcurrentFreezeNoOverdraft(t *testing.T) {
	st := newTestStore(t)
	l := NewLedger(st)
	tokID, uid := seedQuota(t, st, 100)

	const workers = 32
	const delta = int64(10)
	var wg sync.WaitGroup
	okCount := make(chan int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Freeze(tokID, uid, delta); err == nil {
				okCount <- 1
			} else if !errors.Is(err, ErrInsufficientQuota) {
				t.Errorf("并发冻结异常错误: %v", err)
			}
		}()
	}
	wg.Wait()
	close(okCount)

	success := 0
	for range okCount {
		success++
	}
	if success != 10 {
		t.Fatalf("并发冻结成功次数应恰好 10（100/10），实际 %d", success)
	}
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != 0 {
		t.Fatalf("并发冻结后 remain 应恰好 0（无透支），实际 %d", tok.RemainQuota)
	}
	if u := reloadUser(t, st, uid); u.Quota != 0 {
		t.Fatalf("用户余额应同步恰好 0，实际 %d", u.Quota)
	}
}

// TestConcurrentSettleMixed 并发冻结+结算混合压力（-race 冒烟）。
func TestConcurrentSettleMixed(t *testing.T) {
	st := newTestStore(t)
	l := NewLedger(st)
	tokID, uid := seedQuota(t, st, 100_000)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Freeze(tokID, uid, 1_000); err != nil {
				t.Errorf("冻结失败: %v", err)
				return
			}
			if err := l.Settle(tokID, uid, 1_000, 800); err != nil {
				t.Errorf("结算失败: %v", err)
			}
		}()
	}
	wg.Wait()

	// 每轮净扣 1000-200退回 = 800 × 16 = 12800 → remain = 87200。
	if tok := reloadToken(t, st, tokID); tok.RemainQuota != 87_200 {
		t.Fatalf("混合压力后 remain 应 87200，实际 %d", tok.RemainQuota)
	}
}
