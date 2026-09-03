package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// newTestLimiter 构造可手动推进时钟的限流器，返回限流器与推进函数。
func newTestLimiter() (*Limiter, func(time.Duration)) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })
	return l, func(d time.Duration) { now = now.Add(d) }
}

// TestAllowRequestWindowBoundary 请求数窗口边界：达到上限后拒绝，
// Retry-After 指向最早事件滑出窗口的时刻；窗口滑过后自动恢复。
func TestAllowRequestWindowBoundary(t *testing.T) {
	l, advance := newTestLimiter()
	const window = time.Minute

	for i := 0; i < 3; i++ {
		ok, retry := l.AllowRequest("k", window, 3, 0)
		if !ok || retry != 0 {
			t.Fatalf("前 3 次请求应放行，第 %d 次 ok=%v retry=%v", i+1, ok, retry)
		}
	}
	ok, retry := l.AllowRequest("k", window, 3, 0)
	if ok {
		t.Fatal("第 4 次请求应被拒绝")
	}
	// 最早请求在 t=0，窗口 60s：建议等待约 60s（当时钟已推进极小量，略小于 60s）。
	if retry <= 0 || retry > window {
		t.Fatalf("Retry-After 应在 (0, %v] 内，实际 %v", window, retry)
	}
	if diff := window - retry; diff > time.Second {
		t.Fatalf("Retry-After 应接近窗口长度 60s，实际 %v", retry)
	}

	// 窗口未滑过前持续拒绝；滑过后恢复且计数重新开始。
	if ok, _ := l.AllowRequest("k", window, 3, 0); ok {
		t.Fatal("窗口内应持续拒绝")
	}
	advance(window + time.Second)
	for i := 0; i < 3; i++ {
		if ok, _ := l.AllowRequest("k", window, 3, 0); !ok {
			t.Fatalf("窗口滑过后第 %d 次请求应放行", i+1)
		}
	}
	if ok, _ := l.AllowRequest("k", window, 3, 0); ok {
		t.Fatal("新窗口再次达到上限后应拒绝")
	}
}

// TestAllowRequestUnlimitedWhenZero 上限为 0 表示不限（请求数维度）。
func TestAllowRequestUnlimitedWhenZero(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 200; i++ {
		if ok, _ := l.AllowRequest("k", time.Minute, 0, 0); !ok {
			t.Fatalf("上限 0 应不限，第 %d 次被拒", i+1)
		}
	}
}

// TestSuccessCountLimit 成功数维度：成功记录满上限后拒绝新请求，
// 与请求数维度独立计数；窗口滑过后恢复。
func TestSuccessCountLimit(t *testing.T) {
	l, advance := newTestLimiter()
	const window = time.Minute

	// 请求数不限（0），成功数上限 2：两次成功后第 3 次请求被拒。
	for i := 0; i < 2; i++ {
		if ok, _ := l.AllowRequest("k", window, 0, 2); !ok {
			t.Fatalf("第 %d 次请求应放行", i+1)
		}
		l.RecordSuccess("k", window)
	}
	if ok, retry := l.AllowRequest("k", window, 0, 2); ok {
		t.Fatal("成功数达上限后应拒绝新请求")
	} else if retry <= 0 || retry > window {
		t.Fatalf("Retry-After 应在 (0, 窗口] 内，实际 %v", retry)
	}

	// 失败的请求（只占请求数、不记成功）不缓解成功数约束。
	advance(window + time.Second)
	if ok, _ := l.AllowRequest("k", window, 0, 2); !ok {
		t.Fatal("新窗口应放行")
	}
	// 不记成功 → 再次请求仍放行（成功数窗口已清空）。
	if ok, _ := l.AllowRequest("k", window, 0, 2); !ok {
		t.Fatal("未记成功时成功数不应累积")
	}
}

// TestAllowRequestCombinedRetry 请求数与成功数同时超限时，
// Retry-After 取两者中更晚解除者。
func TestAllowRequestCombinedRetry(t *testing.T) {
	l, advance := newTestLimiter()
	const window = time.Minute

	// t=0,1,2s：耗尽请求数上限 3（此阶段成功数不限）。
	for i := 0; i < 3; i++ {
		if ok, _ := l.AllowRequest("k", window, 3, 0); !ok {
			t.Fatalf("预热第 %d 次应放行", i+1)
		}
		advance(time.Second)
	}
	// t=10s：记一次成功（成功数窗口从此刻起算）。
	advance(7 * time.Second)
	l.RecordSuccess("k", window)

	// 请求数约束解除点 = 首请求 t=0 滑出 → 50s；
	// 成功数约束解除点 = 成功记录 t=10s 滑出 → 60s；组合取更晚的 60s。
	_, retry := l.AllowRequest("k", window, 3, 1)
	if retry != window {
		t.Fatalf("组合约束 Retry-After 应取更晚解除者 60s，实际 %v", retry)
	}
}

// TestAllowTokensTPM TPM 边界：窗口内累计 tokens 达上限后拒绝；
// 最早事件滑出窗口、累计降到上限以下后恢复；Retry-After 指向该时刻。
func TestAllowTokensTPM(t *testing.T) {
	l, advance := newTestLimiter()

	if ok, _ := l.AllowTokens("tok", 100, 0); !ok {
		t.Fatal("初始请求应放行")
	}
	l.RecordTokenUsage("tok", 80)

	if ok, _ := l.AllowTokens("tok", 100, 0); !ok {
		t.Fatal("累计 80 < 100 应放行")
	}
	l.RecordTokenUsage("tok", 30) // 累计 110

	// 第二次请求发生在 t≈10s（首次后），最早 token 事件（t=0）过期点 t=60s。
	if ok, retry := l.AllowTokens("tok", 100, 0); ok {
		t.Fatal("累计 110 ≥ 100 应拒绝")
	} else if retry <= 0 || retry > TokenWindow {
		t.Fatalf("TPM Retry-After 应在 (0, %v] 内，实际 %v", TokenWindow, retry)
	}

	// 推进到最早事件（80 tokens）滑出窗口：剩余 30 < 100 → 恢复。
	advance(TokenWindow + time.Second)
	if ok, _ := l.AllowTokens("tok", 100, 0); !ok {
		t.Fatal("最早事件过期后应放行")
	}
}

// TestAllowTokensTPMRetryAfter TPM 的 Retry-After 应指向「剩余用量降到
// 上限以下」的最早时刻，而非简单指向窗口起点。
func TestAllowTokensTPMRetryAfter(t *testing.T) {
	l, advance := newTestLimiter()

	if ok, _ := l.AllowTokens("tok", 100, 0); !ok {
		t.Fatal("首次请求应放行")
	}
	l.RecordTokenUsage("tok", 60)
	advance(5 * time.Second)
	if ok, _ := l.AllowTokens("tok", 100, 0); !ok {
		t.Fatal("累计 60 < 100 应放行")
	}
	l.RecordTokenUsage("tok", 60) // 累计 120，t=5s

	advance(1 * time.Second) // now = t=6s
	_, retry := l.AllowTokens("tok", 100, 0)
	// 移除最早事件（t=0，60 tokens）后剩余 60 < 100 → 最早解除点 = 该事件过期点
	// t=60s，即再等 60-6 = 54s（第二个事件虽更晚，但移除它不是最早充分条件）。
	if retry != 54*time.Second {
		t.Fatalf("期望 Retry-After=54s，实际 %v", retry)
	}
}

// TestAllowTokensRPM RPM 边界：窗口内请求数达上限后拒绝。
func TestAllowTokensRPM(t *testing.T) {
	l, advance := newTestLimiter()

	for i := 0; i < 2; i++ {
		if ok, _ := l.AllowTokens("tok", 0, 2); !ok {
			t.Fatalf("前 %d 次请求应放行", i+1)
		}
	}
	if ok, retry := l.AllowTokens("tok", 0, 2); ok {
		t.Fatal("RPM 达上限后应拒绝")
	} else if retry <= 0 || retry > TokenWindow {
		t.Fatalf("RPM Retry-After 应在 (0, %v] 内，实际 %v", TokenWindow, retry)
	}
	advance(TokenWindow + time.Second)
	if ok, _ := l.AllowTokens("tok", 0, 2); !ok {
		t.Fatal("窗口滑过后应恢复")
	}
}

// TestRecordTokenUsageIgnoredNonPositive 非正 tokens 不应进入窗口。
func TestRecordTokenUsageIgnoredNonPositive(t *testing.T) {
	l, _ := newTestLimiter()
	l.RecordTokenUsage("tok", 0)
	l.RecordTokenUsage("tok", -5)
	if _, _, tokens := l.WindowStats("tok", TokenWindow); tokens != 0 {
		t.Fatalf("非正用量不应累计，实际 %d", tokens)
	}
}

// TestIdleSweep 空闲条目 TTL 清扫：超过 idleTTL 未访问的条目被删除，
// 删除后同 key 重新从空窗口开始计数（内存有界 + 语义回归初始态）。
func TestIdleSweep(t *testing.T) {
	l, advance := newTestLimiter()
	l.maxKeys = 4 // 触发清扫的阈值压到测试量级

	for i := 0; i < 4; i++ {
		l.bucketFor(keyOf(i))
	}
	if l.Len() != 4 {
		t.Fatalf("期望 4 个条目，实际 %d", l.Len())
	}

	// 全部条目空闲超时后新建第 5 个：触发清扫，空闲桶整体删除。
	advance(idleTTL + time.Minute)
	l.bucketFor(keyOf(4))
	if got := l.Len(); got >= 4 {
		t.Fatalf("清扫后条目数应 <4（4 个空闲桶被删），实际 %d", got)
	}
}

// TestSweepKeepsActiveBucket 清扫只删空闲桶：活跃桶（刚访问过）保留且计数不丢。
func TestSweepKeepsActiveBucket(t *testing.T) {
	l, advance := newTestLimiter()
	l.maxKeys = 2

	if ok, _ := l.AllowRequest("hot", time.Minute, 10, 0); !ok {
		t.Fatal("活跃桶请求应放行")
	}
	advance(idleTTL + time.Minute)
	// 先刷新活跃桶，再新建桶触发清扫。
	if ok, _ := l.AllowRequest("hot", time.Minute, 10, 0); !ok {
		t.Fatal("活跃桶刷新请求应放行")
	}
	l.bucketFor("cold")
	if _, ok := l.windows.Load("hot"); !ok {
		t.Fatal("活跃桶不应被清扫删除")
	}
}

// TestConcurrentSameKey 同 key 并发读写（-race 冒烟）：放行总数不超上限、无死锁。
func TestConcurrentSameKey(t *testing.T) {
	l, _ := newTestLimiter()
	const total, limit = 200, 50

	var wg sync.WaitGroup
	allowed := make(chan struct{}, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.AllowRequest("k", time.Minute, limit, 0); ok {
				l.RecordSuccess("k", time.Minute)
				allowed <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(allowed)
	n := len(allowed)
	if n > limit {
		t.Fatalf("并发放行数 %d 不应超过上限 %d", n, limit)
	}
	// 放行与成功记录应一一对应。
	if _, succs, _ := l.WindowStats("k", time.Minute); succs != n {
		t.Fatalf("成功记录数 %d 应等于放行数 %d", succs, n)
	}
}

// keyOf 生成测试用 key。
func keyOf(i int) string { return "k" + string(rune('a'+i)) }
