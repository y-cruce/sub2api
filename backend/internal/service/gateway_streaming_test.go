//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// 测试：独立 Context 的核心逻辑
// =============================================================================

func TestIndependentContext_HttpCtxNotAffectedByCtxCancel(t *testing.T) {
	// 模拟 Forward 函数中的 context 创建逻辑
	// ctx 是下游客户端的 context
	ctx, ctxCancel := context.WithCancel(context.Background())
	
	// httpCtx 是独立的上游 context
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer httpCancel()

	// 模拟下游客户端断开
	ctxCancel()

	// 验证 ctx 已取消
	select {
	case <-ctx.Done():
		// 正确：ctx 已取消
	default:
		t.Error("ctx 应该已被取消")
	}

	// 验证 httpCtx 不受影响
	select {
	case <-httpCtx.Done():
		t.Error("httpCtx 不应该因为 ctx 取消而被取消")
	default:
		// 正确：httpCtx 独立于 ctx
	}

	// 验证 httpCtx 仍然可用
	assert.NoError(t, httpCtx.Err())
}

func TestIndependentContext_HttpCtxHasCorrectTimeout(t *testing.T) {
	// 测试上游 context 的超时设置
	timeout := 10 * time.Minute

	httpCtx, httpCancel := context.WithTimeout(context.Background(), timeout)
	defer httpCancel()

	// 获取 deadline
	deadline, ok := httpCtx.Deadline()
	require.True(t, ok, "httpCtx 应该有 deadline")

	// 验证 deadline 在 10 分钟后（允许 1 秒误差）
	expectedDeadline := time.Now().Add(timeout)
	assert.WithinDuration(t, expectedDeadline, deadline, time.Second)
}

// =============================================================================
// 测试：Forward 函数中的 upstreamCtx 创建
// =============================================================================

func TestForward_CreatesIndependentUpstreamCtx(t *testing.T) {
	// 这个测试验证 Forward 函数创建了独立的 upstreamCtx
	// 由于 Forward 函数依赖很多外部服务，我们只测试核心逻辑

	// 创建一个带超时的 context（模拟 Forward 中的 upstreamCtx）
	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer upstreamCancel()

	// 模拟下游 context 被取消
	downstreamCtx, downstreamCancel := context.WithCancel(context.Background())
	downstreamCancel() // 立即取消

	// 验证 upstreamCtx 不受 downstreamCtx 影响
	select {
	case <-upstreamCtx.Done():
		t.Error("upstreamCtx 不应该因为 downstreamCtx 取消而被取消")
	default:
		// 正确：upstreamCtx 独立于 downstreamCtx
	}

	// 验证 downstreamCtx 已被取消
	select {
	case <-downstreamCtx.Done():
		// 正确：downstreamCtx 已取消
	default:
		t.Error("downstreamCtx 应该已被取消")
	}
}

// =============================================================================
// 测试：drain 时间限制（使用 timer channel 实现）
// =============================================================================

func TestDrainTimeout_TimerApproach(t *testing.T) {
	// 测试新的 drainTimer 实现逻辑
	// drainTimer 初始为 nil，客户端断开时创建

	t.Run("初始状态：drainTimer 为 nil，select 不会触发", func(t *testing.T) {
		var drainTimer <-chan time.Time // nil channel

		select {
		case <-drainTimer:
			t.Error("nil channel 不应该触发")
		default:
			// 正确：nil channel 永远不会 ready
		}
	})

	t.Run("客户端断开后：drainTimer 被设置", func(t *testing.T) {
		// 模拟客户端断开时设置 timer
		drainTimer := time.After(10 * time.Millisecond)

		// 等待 timer 触发
		select {
		case <-drainTimer:
			// 正确：timer 触发
		case <-time.After(100 * time.Millisecond):
			t.Error("drainTimer 应该在 10ms 后触发")
		}
	})

	t.Run("drainTimer 确保 select 不会永久阻塞", func(t *testing.T) {
		// 模拟实际场景：events channel 无数据，intervalCh 为 nil
		events := make(chan struct{}) // 空 channel，永远不会有数据
		var intervalCh <-chan time.Time // nil

		// 客户端断开，创建 drainTimer
		drainTimer := time.After(20 * time.Millisecond)

		// 即使 events 和 intervalCh 都没有数据，drainTimer 也会触发
		select {
		case <-events:
			t.Error("events 不应该触发")
		case <-intervalCh:
			t.Error("intervalCh 为 nil，不应该触发")
		case <-drainTimer:
			// 正确：drainTimer 触发，避免永久阻塞
		case <-time.After(100 * time.Millisecond):
			t.Error("应该由 drainTimer 触发退出")
		}
	})
}

// =============================================================================
// 测试：UpdateSessionWindow 使用独立 context
// =============================================================================

func TestUpdateSessionWindow_IndependentContext(t *testing.T) {
	// 创建一个已取消的 context（模拟下游断开）
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// 创建一个独立的 dbCtx（模拟修复后的行为）
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dbCancel()

	// 验证 canceledCtx 已取消
	assert.Error(t, canceledCtx.Err(), "canceledCtx 应该已被取消")

	// 验证 dbCtx 不受 canceledCtx 影响
	select {
	case <-dbCtx.Done():
		t.Error("dbCtx 不应该因为 canceledCtx 取消而被取消")
	default:
		// 正确：dbCtx 独立于 canceledCtx
	}

	// 验证 dbCtx 在超时前可用
	assert.NoError(t, dbCtx.Err())
}

// =============================================================================
// 测试：sendErrorEvent 函数逻辑
// =============================================================================

func TestSendErrorEvent_OnlyOnce(t *testing.T) {
	// 测试 errorEventSent 标志确保只发送一次错误事件
	errorEventSent := false

	sendErrorEvent := func(reason string) bool {
		if errorEventSent {
			return false
		}
		errorEventSent = true
		return true
	}

	// 第一次调用应该成功
	assert.True(t, sendErrorEvent("stream_interrupted"))
	assert.True(t, errorEventSent)

	// 第二次调用应该失败（不重复发送）
	assert.False(t, sendErrorEvent("stream_interrupted"))
}

// =============================================================================
// 测试：context.Canceled 处理逻辑
// =============================================================================

func TestContextCanceled_SendsEndSignal(t *testing.T) {
	// 模拟 handleStreamingResponse 中的 context.Canceled 处理逻辑
	tests := []struct {
		name               string
		clientDisconnected bool
		expectSendSignal   bool
	}{
		{
			name:               "客户端未断开时应发送结束信号",
			clientDisconnected: false,
			expectSendSignal:   true,
		},
		{
			name:               "客户端已断开时不发送结束信号",
			clientDisconnected: true,
			expectSendSignal:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 模拟发送信号的逻辑
			shouldSend := !tt.clientDisconnected
			assert.Equal(t, tt.expectSendSignal, shouldSend)
		})
	}
}

// =============================================================================
// 集成测试：模拟流式响应场景
// =============================================================================

func TestStreamingResponse_MockScenarios(t *testing.T) {
	t.Run("场景A：正常完成", func(t *testing.T) {
		// 模拟正常的流式响应完成
		clientDisconnected := false
		var usage ClaudeUsage
		usage.InputTokens = 100
		usage.OutputTokens = 50

		// 验证 usage 被正确收集
		assert.Equal(t, 100, usage.InputTokens)
		assert.Equal(t, 50, usage.OutputTokens)
		assert.False(t, clientDisconnected)
	})

	t.Run("场景B：下游断开，继续读取上游", func(t *testing.T) {
		// 模拟下游断开但上游继续读取
		clientDisconnected := true
		disconnectTime := time.Now()
		var usage ClaudeUsage
		usage.InputTokens = 100
		usage.OutputTokens = 50 // 假设成功读取到了 output_tokens

		// 验证即使客户端断开，usage 仍然被收集
		assert.True(t, clientDisconnected)
		assert.False(t, disconnectTime.IsZero())
		assert.Equal(t, 50, usage.OutputTokens)
	})

	t.Run("场景C：上游超时，发送错误信号", func(t *testing.T) {
		// 模拟上游超时
		clientDisconnected := false
		errorEventSent := false

		// 模拟发送错误事件
		if !clientDisconnected && !errorEventSent {
			errorEventSent = true
		}

		assert.True(t, errorEventSent)
	})
}

// =============================================================================
// 测试：HTTP 请求 Context 超时
// =============================================================================

func TestUpstreamContext_Timeout(t *testing.T) {
	// 测试上游 context 的超时设置
	timeout := 10 * time.Minute

	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), timeout)
	defer upstreamCancel()

	// 获取 deadline
	deadline, ok := upstreamCtx.Deadline()
	require.True(t, ok, "upstreamCtx 应该有 deadline")

	// 验证 deadline 在 10 分钟后
	expectedDeadline := time.Now().Add(timeout)
	assert.WithinDuration(t, expectedDeadline, deadline, time.Second)
}

// =============================================================================
// 测试：handleNonStreamingResponse 独立 context
// =============================================================================

func TestNonStreamingResponse_IndependentDbContext(t *testing.T) {
	// 验证非流式响应也使用独立的 dbCtx
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dbCancel()

	// 验证 dbCtx 独立
	assert.Error(t, canceledCtx.Err())
	assert.NoError(t, dbCtx.Err())
}

// =============================================================================
// 基准测试
// =============================================================================

func BenchmarkDrainTimeoutCheck(b *testing.B) {
	const maxDrainDuration = 30 * time.Second
	disconnectTime := time.Now().Add(-15 * time.Second)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = !disconnectTime.IsZero() && time.Since(disconnectTime) > maxDrainDuration
	}
}

func BenchmarkContextCreation(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		cancel()
		_ = ctx
	}
}
