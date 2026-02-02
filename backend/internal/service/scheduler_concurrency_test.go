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
// Mock 调度器缓存
// =============================================================================

type mockSchedulerCache struct {
	mu        sync.RWMutex
	snapshots map[string][]*Account
	accounts  map[int64]*Account
	delay     time.Duration // 模拟缓存延迟
}

func newMockSchedulerCache() *mockSchedulerCache {
	return &mockSchedulerCache{
		snapshots: make(map[string][]*Account),
		accounts:  make(map[int64]*Account),
	}
}

func (m *mockSchedulerCache) GetSnapshot(ctx context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	accounts, ok := m.snapshots[bucket.String()]
	if !ok {
		return nil, false, nil
	}
	return accounts, true, nil
}

func (m *mockSchedulerCache) SetSnapshot(ctx context.Context, bucket SchedulerBucket, accounts []Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ptrs := make([]*Account, len(accounts))
	for i := range accounts {
		copied := accounts[i]
		ptrs[i] = &copied
	}
	m.snapshots[bucket.String()] = ptrs
	return nil
}

func (m *mockSchedulerCache) GetAccount(ctx context.Context, id int64) (*Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accounts[id], nil
}

func (m *mockSchedulerCache) SetAccount(ctx context.Context, account *Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[account.ID] = account
	return nil
}

func (m *mockSchedulerCache) DeleteAccount(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.accounts, id)
	return nil
}

func (m *mockSchedulerCache) TryLockBucket(ctx context.Context, bucket SchedulerBucket, ttl time.Duration) (bool, error) {
	return true, nil
}

func (m *mockSchedulerCache) ListBuckets(ctx context.Context) ([]SchedulerBucket, error) {
	return nil, nil
}

func (m *mockSchedulerCache) SetOutboxWatermark(ctx context.Context, watermark int64) error {
	return nil
}

func (m *mockSchedulerCache) GetOutboxWatermark(ctx context.Context) (int64, error) {
	return 0, nil
}

func (m *mockSchedulerCache) UpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	return nil
}

// =============================================================================
// 测试数据构建
// =============================================================================

func buildTestAccounts(count int, platform string) []Account {
	accounts := make([]Account, count)
	for i := 0; i < count; i++ {
		accounts[i] = Account{
			ID:          int64(i + 1),
			Name:        "Account " + string(rune('A'+i)),
			Platform:    platform,
			Type:        "api_key",
			Status:      StatusActive,
			Schedulable: true,
		}
	}
	return accounts
}

func buildTestAccountsWithRateLimits(count int, platform string, rateLimitedCount int) []Account {
	accounts := buildTestAccounts(count, platform)
	future := time.Now().Add(5 * time.Minute)
	for i := 0; i < rateLimitedCount && i < count; i++ {
		accounts[i].RateLimitResetAt = &future
	}
	return accounts
}

// =============================================================================
// 测试用例：并发请求场景
// =============================================================================

func TestSchedulerConcurrency_MultipleRequests_NoRaceCondition(t *testing.T) {
	// 模拟场景：10个账号，50个并发请求
	cache := newMockSchedulerCache()

	accounts := buildTestAccounts(10, PlatformAnthropic)
	// Anthropic 默认使用 Mixed 模式
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}
	err := cache.SetSnapshot(context.Background(), bucket, accounts)
	require.NoError(t, err)

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	var wg sync.WaitGroup
	concurrency := 50
	successCount := int32(0)
	errorCount := int32(0)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			result, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)
			if err != nil {
				atomic.AddInt32(&errorCount, 1)
				return
			}
			if len(result) > 0 {
				atomic.AddInt32(&successCount, 1)
			} else {
				atomic.AddInt32(&errorCount, 1)
			}
		}()
	}

	wg.Wait()

	t.Logf("Results: success=%d, error=%d", successCount, errorCount)
	require.Equal(t, int32(concurrency), successCount, "All requests should succeed")
	require.Equal(t, int32(0), errorCount, "No requests should fail")
}

func TestSchedulerConcurrency_CacheDelay_StillWorks(t *testing.T) {
	// 模拟场景：缓存有延迟，但请求仍应成功
	cache := newMockSchedulerCache()
	cache.delay = 10 * time.Millisecond // 模拟缓存延迟

	accounts := buildTestAccounts(5, PlatformAnthropic)
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}
	err := cache.SetSnapshot(context.Background(), bucket, accounts)
	require.NoError(t, err)

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	var wg sync.WaitGroup
	concurrency := 20
	successCount := int32(0)

	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			result, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)
			if err == nil && len(result) > 0 {
				atomic.AddInt32(&successCount, 1)
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("Elapsed: %v, success=%d", elapsed, successCount)
	require.Equal(t, int32(concurrency), successCount)
}

func TestSchedulerConcurrency_PartialRateLimited_SomeAvailable(t *testing.T) {
	// 模拟场景：部分账号被限流，但还有账号可用
	cache := newMockSchedulerCache()

	// 10个账号，其中5个被限流
	accounts := buildTestAccountsWithRateLimits(10, PlatformAnthropic, 5)
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}

	// 缓存中存储所有账号（包括限流的）
	err := cache.SetSnapshot(context.Background(), bucket, accounts)
	require.NoError(t, err)

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	ctx := context.Background()
	result, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)
	require.NoError(t, err)

	// 缓存返回所有账号，调用方需要再次过滤 IsSchedulable
	t.Logf("Returned accounts: %d", len(result))

	// 检查有多少账号实际可调度
	schedulableCount := 0
	for _, acc := range result {
		if acc.IsSchedulable() {
			schedulableCount++
		}
	}
	t.Logf("Actually schedulable: %d", schedulableCount)

	// 关键问题：如果缓存返回了所有账号，但调用方过滤后发现没有可用的，
	// 就会出现 "no available accounts" 错误
}

func TestSchedulerConcurrency_AllRateLimited_NoAvailable(t *testing.T) {
	// 模拟场景：所有账号都被限流
	cache := newMockSchedulerCache()

	// 5个账号，全部被限流
	accounts := buildTestAccountsWithRateLimits(5, PlatformAnthropic, 5)
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}
	err := cache.SetSnapshot(context.Background(), bucket, accounts)
	require.NoError(t, err)

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	ctx := context.Background()
	result, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)
	require.NoError(t, err)

	// 缓存返回了账号，但实际都不可调度
	t.Logf("Returned accounts: %d", len(result))
	require.Equal(t, 5, len(result), "Cache returns all accounts")

	schedulableCount := 0
	for _, acc := range result {
		if acc.IsSchedulable() {
			schedulableCount++
		}
	}
	require.Equal(t, 0, schedulableCount, "None should be schedulable due to rate limits")
}

func TestSchedulerConcurrency_EmptyCache_NoFallback(t *testing.T) {
	// 模拟场景：缓存为空，且没有配置 fallback
	cache := newMockSchedulerCache()
	// 不设置任何快照

	svc := &SchedulerSnapshotService{
		cache: cache,
		// 没有配置 accountRepo，无法 fallback 到数据库
	}

	ctx := context.Background()
	result, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)

	// 没有缓存且没有 fallback 配置，应该返回错误
	t.Logf("Result: accounts=%d, err=%v", len(result), err)

	// 这种情况下，缓存 miss 后会尝试 fallback，但如果没有配置会失败
	if err == nil {
		require.Empty(t, result, "Should return empty when cache miss and no fallback")
	}
}

// =============================================================================
// 测试用例：模拟页面显示空闲但API返回无可用的问题
// =============================================================================

func TestSchedulerConcurrency_DisplayVsActual_Mismatch(t *testing.T) {
	// 模拟场景：页面显示有空闲账号，但 API 返回无可用
	// 原因可能是：
	// 1. 页面读取的是数据库状态
	// 2. API 使用的是缓存快照
	// 3. 缓存快照过时或未包含最新的账号状态

	cache := newMockSchedulerCache()

	// 模拟数据库中有5个可用账号
	dbAccounts := buildTestAccounts(5, PlatformAnthropic)
	_ = dbAccounts // 数据库账号存在但未被缓存

	// 不设置缓存，模拟缓存未初始化

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	ctx := context.Background()
	result, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)

	t.Logf("DB accounts: %d", len(dbAccounts))
	t.Logf("Cache result: accounts=%d, err=%v", len(result), err)

	// 这展示了问题：即使数据库有账号，如果缓存未命中且没有 fallback，
	// 就会返回空或错误
}

func TestSchedulerConcurrency_StaleCache_ProblemRepro(t *testing.T) {
	// 模拟场景：缓存中的账号状态过时
	cache := newMockSchedulerCache()

	// 初始状态：5个可用账号
	accounts := buildTestAccounts(5, PlatformAnthropic)
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}
	err := cache.SetSnapshot(context.Background(), bucket, accounts)
	require.NoError(t, err)

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	// 第一次请求：应该成功
	ctx := context.Background()
	result1, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)
	require.NoError(t, err)
	require.Len(t, result1, 5)
	t.Logf("First request: %d accounts", len(result1))

	// 模拟：所有账号都被限流了（在实际系统中，这会更新数据库和发送 outbox 事件）
	// 但缓存可能还没更新
	rateLimitedAccounts := buildTestAccountsWithRateLimits(5, PlatformAnthropic, 5)
	err = cache.SetSnapshot(context.Background(), bucket, rateLimitedAccounts)
	require.NoError(t, err)

	// 第二次请求：缓存返回账号，但都被限流
	result2, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)
	require.NoError(t, err)
	require.Len(t, result2, 5) // 缓存返回5个

	// 检查实际可调度的数量
	schedulable := 0
	for _, acc := range result2 {
		if acc.IsSchedulable() {
			schedulable++
		}
	}
	t.Logf("Second request: returned=%d, actually_schedulable=%d", len(result2), schedulable)
	require.Equal(t, 0, schedulable, "All accounts rate limited")

	// 这就是问题所在：
	// - 缓存返回了 5 个账号
	// - 调用方过滤后发现 0 个可调度
	// - 返回 "no available accounts" 错误
	// - 但页面可能显示这 5 个账号是"空闲"的（如果页面没有检查 RateLimitResetAt）
}

// =============================================================================
// 测试用例：高并发下的竞争条件
// =============================================================================

func TestSchedulerConcurrency_HighLoad_RaceCondition(t *testing.T) {
	// 模拟场景：高并发请求 + 账号状态频繁变化
	cache := newMockSchedulerCache()

	accounts := buildTestAccounts(10, PlatformAnthropic)
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}
	err := cache.SetSnapshot(context.Background(), bucket, accounts)
	require.NoError(t, err)

	svc := &SchedulerSnapshotService{
		cache: cache,
	}

	var wg sync.WaitGroup
	concurrency := 100
	successCount := int32(0)
	emptyCount := int32(0)
	errorCount := int32(0)

	// 同时启动多个请求
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			result, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformAnthropic, false)
			if err != nil {
				atomic.AddInt32(&errorCount, 1)
				return
			}
			if len(result) == 0 {
				atomic.AddInt32(&emptyCount, 1)
			} else {
				atomic.AddInt32(&successCount, 1)
			}
		}()
	}

	// 同时模拟缓存更新（账号状态变化）
	go func() {
		for i := 0; i < 10; i++ {
			time.Sleep(1 * time.Millisecond)
			// 随机限流一些账号
			updated := buildTestAccountsWithRateLimits(10, PlatformAnthropic, i)
			cache.SetSnapshot(context.Background(), bucket, updated)
		}
	}()

	wg.Wait()

	t.Logf("Results: success=%d, empty=%d, error=%d", successCount, emptyCount, errorCount)

	// 在正常情况下，即使有竞争，大部分请求应该成功
	// 如果 emptyCount 很高，说明存在问题
	require.True(t, successCount > int32(concurrency/2),
		"At least half of requests should succeed, got %d/%d", successCount, concurrency)
}

// =============================================================================
// 测试用例：验证 IsSchedulable 过滤逻辑
// =============================================================================

func TestAccountIsSchedulable_AllConditions(t *testing.T) {
	now := time.Now()
	future := now.Add(10 * time.Minute)
	past := now.Add(-10 * time.Minute)

	tests := []struct {
		name           string
		account        Account
		expectSchedule bool
	}{
		{
			name: "active and schedulable",
			account: Account{
				Status:      StatusActive,
				Schedulable: true,
			},
			expectSchedule: true,
		},
		{
			name: "disabled status",
			account: Account{
				Status:      StatusDisabled,
				Schedulable: true,
			},
			expectSchedule: false,
		},
		{
			name: "not schedulable flag",
			account: Account{
				Status:      StatusActive,
				Schedulable: false,
			},
			expectSchedule: false,
		},
		{
			name: "rate limited - future",
			account: Account{
				Status:           StatusActive,
				Schedulable:      true,
				RateLimitResetAt: &future,
			},
			expectSchedule: false,
		},
		{
			name: "rate limited - past (expired)",
			account: Account{
				Status:           StatusActive,
				Schedulable:      true,
				RateLimitResetAt: &past,
			},
			expectSchedule: true,
		},
		{
			name: "temp unschedulable - future",
			account: Account{
				Status:               StatusActive,
				Schedulable:          true,
				TempUnschedulableUntil: &future,
			},
			expectSchedule: false,
		},
		{
			name: "overload until - future",
			account: Account{
				Status:        StatusActive,
				Schedulable:   true,
				OverloadUntil: &future,
			},
			expectSchedule: false,
		},
		{
			name: "expired account with auto pause",
			account: Account{
				Status:            StatusActive,
				Schedulable:       true,
				AutoPauseOnExpired: true,
				ExpiresAt:         &past,
			},
			expectSchedule: false,
		},
		{
			name: "expired account without auto pause",
			account: Account{
				Status:            StatusActive,
				Schedulable:       true,
				AutoPauseOnExpired: false,
				ExpiresAt:         &past,
			},
			expectSchedule: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.account.IsSchedulable()
			require.Equal(t, tt.expectSchedule, result)
		})
	}
}
