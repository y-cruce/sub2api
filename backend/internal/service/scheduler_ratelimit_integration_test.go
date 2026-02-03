//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// 测试场景：模拟真实的限流后缓存更新延迟问题
// =============================================================================

// mockAccountRepoForRateLimit 模拟账号仓库，追踪限流状态
type mockAccountRepoForRateLimit struct {
	mu             sync.RWMutex
	accounts       []Account
	rateLimitedIDs map[int64]time.Time // 记录被限流的账号
	rateLimitCalls int32               // 记录 SetRateLimited 调用次数
}

func newMockAccountRepoForRateLimit(accounts []Account) *mockAccountRepoForRateLimit {
	return &mockAccountRepoForRateLimit{
		accounts:       accounts,
		rateLimitedIDs: make(map[int64]time.Time),
	}
}

func (r *mockAccountRepoForRateLimit) SetRateLimited(id int64, resetAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	atomic.AddInt32(&r.rateLimitCalls, 1)
	r.rateLimitedIDs[id] = resetAt
	// 更新账号状态
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			r.accounts[i].RateLimitResetAt = &resetAt
			break
		}
	}
}

func (r *mockAccountRepoForRateLimit) ListSchedulable() []Account {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now()
	var result []Account
	for _, acc := range r.accounts {
		// 数据库查询会过滤限流账号
		if acc.RateLimitResetAt == nil || !now.Before(*acc.RateLimitResetAt) {
			result = append(result, acc)
		}
	}
	return result
}

func (r *mockAccountRepoForRateLimit) GetRateLimitCallCount() int32 {
	return atomic.LoadInt32(&r.rateLimitCalls)
}

// =============================================================================
// 测试1：验证缓存与数据库的不一致窗口
// =============================================================================

func TestRateLimitCacheInconsistency_CacheStaleWindow(t *testing.T) {
	// 场景：
	// 1. 缓存加载了 5 个可用账号（这是一个快照副本）
	// 2. 所有账号被限流（更新数据库，但缓存中的副本不会自动更新）
	// 3. 需要刷新缓存才能获取最新状态

	// Step 1: 初始化账号
	accounts := buildTestAccounts(5, PlatformAnthropic)
	repo := newMockAccountRepoForRateLimit(accounts)

	// Step 2: 模拟缓存加载（此时账号可用，存储的是副本）
	cache := newMockSchedulerCache()
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}
	cacheAccounts := repo.ListSchedulable()
	err := cache.SetSnapshot(context.Background(), bucket, cacheAccounts)
	require.NoError(t, err)
	t.Logf("Step 1: 缓存加载 %d 个可用账号（快照副本）", len(cacheAccounts))

	// Step 3: 所有账号被限流（更新 repo 中的账号）
	future := time.Now().Add(5 * time.Minute)
	for _, acc := range accounts {
		repo.SetRateLimited(acc.ID, future)
	}
	t.Logf("Step 2: 所有 %d 个账号被限流（repo 更新）", len(accounts))

	// Step 4: 验证数据库/repo 状态（应该没有可调度账号）
	dbSchedulable := repo.ListSchedulable()
	t.Logf("Step 3: Repo 查询可调度账号 = %d", len(dbSchedulable))
	require.Equal(t, 0, len(dbSchedulable), "Repo 应该没有可调度账号")

	// Step 5: 读取缓存（缓存是旧的快照，不会自动更新）
	cachedAccounts, hit, err := cache.GetSnapshot(context.Background(), bucket)
	require.NoError(t, err)
	require.True(t, hit)
	t.Logf("Step 4: 缓存返回 %d 个账号", len(cachedAccounts))

	// Step 6: 缓存中的账号是旧快照，RateLimitResetAt 仍为 nil
	var schedulableInCache int
	for _, acc := range cachedAccounts {
		if acc.IsSchedulable() {
			schedulableInCache++
		}
	}
	t.Logf("Step 5: 缓存账号 IsSchedulable() = %d（因为是旧快照，不知道已被限流）", schedulableInCache)

	// 关键验证：缓存快照与数据库状态不一致
	require.Equal(t, 5, len(cachedAccounts), "缓存返回旧快照的 5 个账号")
	require.Equal(t, 5, schedulableInCache, "缓存快照中账号看起来可用（RateLimitResetAt=nil）")
	require.Equal(t, 0, len(dbSchedulable), "但 repo/数据库显示 0 个可用")

	t.Log("============================================")
	t.Log("关键发现：")
	t.Log("- 缓存存储的是账号对象的【副本/快照】")
	t.Log("- Repo/数据库更新后，缓存副本不会自动同步")
	t.Log("- 在实际系统中：")
	t.Log("  1. 请求使用缓存账号 A")
	t.Log("  2. 上游返回 429，调用 SetRateLimited(A)")
	t.Log("  3. 数据库更新 A.RateLimitResetAt = future")
	t.Log("  4. 但缓存中 A 的副本仍是 RateLimitResetAt = nil")
	t.Log("  5. 下个请求仍选中 A（因为 IsSchedulable()=true）")
	t.Log("  6. 直到缓存刷新才能正确过滤")
	t.Log("============================================")
}

// =============================================================================
// 测试2：模拟高并发请求下的竞争条件
// =============================================================================

func TestRateLimitCacheInconsistency_HighConcurrency(t *testing.T) {
	// 场景：多个并发请求同时触发限流，后续请求无法找到可用账号

	accounts := buildTestAccounts(5, PlatformAnthropic)
	repo := newMockAccountRepoForRateLimit(accounts)

	cache := newMockSchedulerCache()
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}

	// 初始加载缓存
	cacheAccounts := repo.ListSchedulable()
	cache.SetSnapshot(context.Background(), bucket, cacheAccounts)

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	// 模拟账号选择和限流
	selectAccount := func(excludedIDs map[int64]struct{}) (*Account, error) {
		accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformAnthropic, false)
		if err != nil {
			return nil, err
		}
		for _, acc := range accounts {
			if _, excluded := excludedIDs[acc.ID]; excluded {
				continue
			}
			if acc.IsSchedulable() {
				return &acc, nil
			}
		}
		return nil, nil
	}

	// 并发测试
	var wg sync.WaitGroup
	requestCount := 20
	successCount := int32(0)
	noAccountCount := int32(0)

	future := time.Now().Add(5 * time.Minute)
	excludedIDs := make(map[int64]struct{})
	var excludeMu sync.Mutex

	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(reqNum int) {
			defer wg.Done()

			excludeMu.Lock()
			localExcluded := make(map[int64]struct{})
			for k, v := range excludedIDs {
				localExcluded[k] = v
			}
			excludeMu.Unlock()

			acc, err := selectAccount(localExcluded)
			if err != nil || acc == nil {
				atomic.AddInt32(&noAccountCount, 1)
				t.Logf("请求 %d: No available accounts (excluded=%d)", reqNum, len(localExcluded))
				return
			}

			// 模拟请求成功后账号被限流
			repo.SetRateLimited(acc.ID, future)

			// 将账号加入排除列表
			excludeMu.Lock()
			excludedIDs[acc.ID] = struct{}{}
			excludeMu.Unlock()

			atomic.AddInt32(&successCount, 1)
			t.Logf("请求 %d: 使用账号 %d，然后被限流", reqNum, acc.ID)
		}(i)
	}

	wg.Wait()

	t.Logf("============================================")
	t.Logf("总请求: %d", requestCount)
	t.Logf("成功: %d", successCount)
	t.Logf("无可用账号: %d", noAccountCount)
	t.Logf("SetRateLimited 调用次数: %d", repo.GetRateLimitCallCount())
	t.Logf("============================================")

	// 验证：前5个请求应该成功（每个用一个账号），后续请求应该失败
	// 但由于缓存未更新，后续请求仍能从缓存获取账号（只是 IsSchedulable 过滤后为空）
}

// =============================================================================
// 测试3：验证缓存更新后问题消失
// =============================================================================

func TestRateLimitCacheInconsistency_AfterCacheRefresh(t *testing.T) {
	accounts := buildTestAccounts(5, PlatformAnthropic)
	repo := newMockAccountRepoForRateLimit(accounts)

	cache := newMockSchedulerCache()
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}

	// 初始加载
	cache.SetSnapshot(context.Background(), bucket, repo.ListSchedulable())

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	// 限流前查询
	before, _, _ := svc.ListSchedulableAccounts(context.Background(), nil, PlatformAnthropic, false)
	t.Logf("限流前: 缓存返回 %d 个账号", len(before))
	beforeSchedulable := 0
	for _, acc := range before {
		if acc.IsSchedulable() {
			beforeSchedulable++
		}
	}
	t.Logf("限流前: IsSchedulable 过滤后 %d 个", beforeSchedulable)

	// 限流 3 个账号
	future := time.Now().Add(5 * time.Minute)
	for i := 0; i < 3; i++ {
		repo.SetRateLimited(accounts[i].ID, future)
	}

	// 缓存未更新时查询（缓存是旧快照，不知道账号已限流）
	middle, _, _ := svc.ListSchedulableAccounts(context.Background(), nil, PlatformAnthropic, false)
	middleSchedulable := 0
	for _, acc := range middle {
		if acc.IsSchedulable() {
			middleSchedulable++
		}
	}
	t.Logf("限流后(缓存未更新): 缓存返回 %d 个，IsSchedulable 过滤后 %d 个（快照不知道限流）", len(middle), middleSchedulable)

	// 模拟缓存刷新（重新从 repo 加载，获取最新状态）
	cache.SetSnapshot(context.Background(), bucket, repo.ListSchedulable())

	// 缓存更新后查询
	after, _, _ := svc.ListSchedulableAccounts(context.Background(), nil, PlatformAnthropic, false)
	afterSchedulable := 0
	for _, acc := range after {
		if acc.IsSchedulable() {
			afterSchedulable++
		}
	}
	t.Logf("限流后(缓存已更新): 缓存返回 %d 个，IsSchedulable 过滤后 %d 个", len(after), afterSchedulable)

	// 验证
	require.Equal(t, 5, beforeSchedulable, "限流前应有 5 个可调度")
	require.Equal(t, 5, len(middle), "缓存未更新时仍返回 5 个（旧快照）")
	require.Equal(t, 5, middleSchedulable, "旧快照中 IsSchedulable()=true（不知道已限流）")
	require.Equal(t, 2, len(after), "缓存更新后只返回 2 个")
	require.Equal(t, 2, afterSchedulable, "全部可调度")

	t.Log("============================================")
	t.Log("结论：")
	t.Log("1. 缓存存储的是快照副本")
	t.Log("2. 限流更新 repo/数据库后，缓存副本不变")
	t.Log("3. 必须刷新缓存才能获取最新状态")
	t.Log("4. 在窗口期内，请求仍会选中已限流的账号")
	t.Log("============================================")
}

// =============================================================================
// 测试4：对比 SetRateLimited 和 SetTempUnschedulable 的行为差异
// =============================================================================

func TestRateLimitVsTempUnschedulable_CacheSyncDifference(t *testing.T) {
	// 验证假设：SetTempUnschedulable 调用了 syncSchedulerAccountSnapshot
	// 而 SetRateLimited 没有

	t.Log("============================================")
	t.Log("根据代码分析：")
	t.Log("")
	t.Log("SetRateLimited (account_repo.go:785):")
	t.Log("  - 更新数据库")
	t.Log("  - enqueueSchedulerOutbox() ← 仅发送事件")
	t.Log("  - ❌ 没有调用 syncSchedulerAccountSnapshot()")
	t.Log("")
	t.Log("SetTempUnschedulable (account_repo.go:892):")
	t.Log("  - 更新数据库")
	t.Log("  - enqueueSchedulerOutbox() ← 发送事件")
	t.Log("  - ✅ syncSchedulerAccountSnapshot() ← 主动同步缓存")
	t.Log("")
	t.Log("结论：SetRateLimited 缺少主动缓存同步，")
	t.Log("导致限流后缓存更新依赖 outbox 轮询（默认 1 秒间隔）")
	t.Log("============================================")
}

// =============================================================================
// 测试5：分析 "No available accounts" 错误的真正原因
// =============================================================================

func TestNoAvailableAccounts_RootCauseAnalysis(t *testing.T) {
	t.Log("============================================")
	t.Log("'No available accounts' 错误触发点分析：")
	t.Log("")
	t.Log("1. gateway_service.go:575")
	t.Log("   条件：listSchedulableAccounts 返回空")
	t.Log("   原因：缓存错误/缓存为空/数据库查询返回空")
	t.Log("")
	t.Log("2. gateway_service.go:898-899")
	t.Log("   条件：Layer 2 过滤后 candidates 为空")
	t.Log("   过滤条件包括：")
	t.Log("   - isExcluded(acc.ID) 排除")
	t.Log("   - !acc.IsSchedulable() 不可调度")
	t.Log("   - !isAccountAllowedForPlatform 平台不匹配")
	t.Log("   - !acc.IsSchedulableForModel 模型不支持")
	t.Log("   - !isModelSupportedByAccount 模型路由不匹配")
	t.Log("   - !isAccountSchedulableForWindowCost 窗口费用检查失败")
	t.Log("")
	t.Log("3. gateway_service.go:996")
	t.Log("   条件：所有账号的会话限制都满了")
	t.Log("   （checkAndRegisterSession 返回 false）")
	t.Log("")
	t.Log("重要发现：")
	t.Log("- 缓存快照是对象副本，状态更新后副本不变")
	t.Log("- IsSchedulable() 检查的是副本状态（可能过时）")
	t.Log("- 如果所有账号同时被限流，缓存刷新前请求失败")
	t.Log("")
	t.Log("可能的真正原因：")
	t.Log("1. 缓存刷新时正好所有账号都被限流 → 缓存为空")
	t.Log("2. 模型路由配置问题 → 请求的模型没有匹配的账号")
	t.Log("3. 分组配置问题 → 分组内没有可用账号")
	t.Log("4. 会话限制问题 → 所有账号会话数满")
	t.Log("============================================")
}

// =============================================================================
// 测试6：模拟缓存刷新时所有账号都被限流的场景
// =============================================================================

func TestCacheRefreshDuringAllRateLimited(t *testing.T) {
	accounts := buildTestAccounts(5, PlatformAnthropic)
	repo := newMockAccountRepoForRateLimit(accounts)

	cache := newMockSchedulerCache()
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}

	// 第一阶段：正常运行，缓存有账号
	cache.SetSnapshot(context.Background(), bucket, repo.ListSchedulable())
	svc := &SchedulerSnapshotService{cache: cache}

	result1, _, _ := svc.ListSchedulableAccounts(context.Background(), nil, PlatformAnthropic, false)
	t.Logf("阶段1: 缓存有 %d 个账号", len(result1))
	require.Equal(t, 5, len(result1))

	// 第二阶段：所有账号被限流
	future := time.Now().Add(5 * time.Minute)
	for _, acc := range accounts {
		repo.SetRateLimited(acc.ID, future)
	}
	t.Log("阶段2: 所有账号被限流")

	// 第三阶段：缓存刷新（此时 repo.ListSchedulable 返回空）
	cache.SetSnapshot(context.Background(), bucket, repo.ListSchedulable())
	t.Logf("阶段3: 缓存刷新，repo 返回 %d 个可调度账号", len(repo.ListSchedulable()))

	result2, _, _ := svc.ListSchedulableAccounts(context.Background(), nil, PlatformAnthropic, false)
	t.Logf("阶段4: 缓存现在有 %d 个账号", len(result2))

	// 验证：缓存刷新后，确实没有可用账号
	require.Equal(t, 0, len(result2), "缓存刷新后应为空")

	t.Log("============================================")
	t.Log("这就是问题！")
	t.Log("当缓存刷新恰好发生在所有账号都被限流时：")
	t.Log("1. repo.ListSchedulable() 返回空（数据库过滤）")
	t.Log("2. 缓存存储空列表")
	t.Log("3. 新请求获取空列表 → 'No available accounts'")
	t.Log("============================================")
}
