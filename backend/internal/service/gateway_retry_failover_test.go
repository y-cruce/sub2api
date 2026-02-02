//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// 测试数据构建
// =============================================================================

// buildAccountWithRetryRules 构建带有重试规则的测试账号
func buildAccountWithRetryRules(rules []TempUnschedulableRule) *Account {
	rulesData := make([]any, 0, len(rules))
	for _, r := range rules {
		ruleMap := map[string]any{
			"error_code":       float64(r.ErrorCode),
			"keywords":         toAnySlice(r.Keywords),
			"duration_minutes": float64(r.DurationMinutes),
			"duration_seconds": float64(r.DurationSeconds),
			"description":      r.Description,
			"retry_enabled":    r.RetryEnabled,
			"retry_count":      float64(r.RetryCount),
		}
		rulesData = append(rulesData, ruleMap)
	}

	return &Account{
		ID:       1,
		Platform: "anthropic",
		Credentials: map[string]any{
			"temp_unschedulable_enabled": true,
			"temp_unschedulable_rules":   rulesData,
		},
	}
}

func toAnySlice(strs []string) []any {
	result := make([]any, len(strs))
	for i, s := range strs {
		result[i] = s
	}
	return result
}

// =============================================================================
// 测试用例：规则匹配逻辑
// =============================================================================

func TestGetRetryRuleForError_MatchByStatusCode(t *testing.T) {
	account := buildAccountWithRetryRules([]TempUnschedulableRule{
		{
			ErrorCode:       429,
			Keywords:        []string{"rate limit", "too many requests"},
			DurationSeconds: 30,
			RetryEnabled:    true,
			RetryCount:      3,
		},
	})

	tests := []struct {
		name         string
		statusCode   int
		responseBody string
		expectMatch  bool
	}{
		{
			name:         "429 with matching keyword",
			statusCode:   429,
			responseBody: `{"error": {"message": "rate limit exceeded"}}`,
			expectMatch:  true,
		},
		{
			name:         "429 with another matching keyword",
			statusCode:   429,
			responseBody: `{"error": {"message": "too many requests, please slow down"}}`,
			expectMatch:  true,
		},
		{
			name:         "429 without matching keyword",
			statusCode:   429,
			responseBody: `{"error": {"message": "unknown error"}}`,
			expectMatch:  false,
		},
		{
			name:         "500 error (wrong status code)",
			statusCode:   500,
			responseBody: `{"error": {"message": "rate limit exceeded"}}`,
			expectMatch:  false,
		},
		{
			name:         "429 with empty body",
			statusCode:   429,
			responseBody: ``,
			expectMatch:  false,
		},
	}

	gs := &GatewayService{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, keyword := gs.getRetryRuleForError(account, tt.statusCode, []byte(tt.responseBody))
			if tt.expectMatch {
				require.NotNil(t, rule, "expected rule to match")
				require.NotEmpty(t, keyword, "expected keyword to be set")
			} else {
				require.Nil(t, rule, "expected no rule match")
			}
		})
	}
}

func TestGetRetryRuleForError_MultipleRules(t *testing.T) {
	account := buildAccountWithRetryRules([]TempUnschedulableRule{
		{
			ErrorCode:       429,
			Keywords:        []string{"rate limit"},
			DurationSeconds: 30,
			RetryEnabled:    true,
			RetryCount:      3,
			Description:     "Rate limit rule",
		},
		{
			ErrorCode:       529,
			Keywords:        []string{"overloaded"},
			DurationMinutes: 5,
			RetryEnabled:    true, // 启用重试才能被 getRetryRuleForError 匹配
			RetryCount:      2,
			Description:     "Overload rule with retry",
		},
		{
			ErrorCode:       500,
			Keywords:        []string{"internal error"},
			DurationSeconds: 60,
			RetryEnabled:    true,
			RetryCount:      2,
			Description:     "Internal error rule",
		},
	})

	tests := []struct {
		name                string
		statusCode          int
		responseBody        string
		expectMatch         bool
		expectRetryEnabled  bool
		expectRetryCount    int
		expectDurationSec   int
	}{
		{
			name:               "429 rate limit - matches first rule with retry",
			statusCode:         429,
			responseBody:       `{"error": "rate limit exceeded"}`,
			expectMatch:        true,
			expectRetryEnabled: true,
			expectRetryCount:   3,
			expectDurationSec:  30,
		},
		{
			name:               "529 overloaded - matches second rule with retry",
			statusCode:         529,
			responseBody:       `{"error": "server overloaded"}`,
			expectMatch:        true,
			expectRetryEnabled: true,
			expectRetryCount:   2,
			expectDurationSec:  300, // 5 * 60
		},
		{
			name:               "500 internal error - matches third rule",
			statusCode:         500,
			responseBody:       `{"error": "internal error occurred"}`,
			expectMatch:        true,
			expectRetryEnabled: true,
			expectRetryCount:   2,
			expectDurationSec:  60,
		},
	}

	gs := &GatewayService{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// getRetryRuleForError 只返回启用了重试的规则
			rule, _ := gs.getRetryRuleForError(account, tt.statusCode, []byte(tt.responseBody))
			if tt.expectMatch {
				require.NotNil(t, rule)
				require.Equal(t, tt.expectRetryEnabled, rule.RetryEnabled)
				require.Equal(t, tt.expectRetryCount, rule.GetRetryCount())
				require.Equal(t, tt.expectDurationSec, rule.GetDurationSeconds())
			} else {
				require.Nil(t, rule)
			}
		})
	}
}

// TestGetRetryRuleForError_NoRetryRuleSkipped 测试禁用重试的规则不会被 getRetryRuleForError 返回
func TestGetRetryRuleForError_NoRetryRuleSkipped(t *testing.T) {
	account := buildAccountWithRetryRules([]TempUnschedulableRule{
		{
			ErrorCode:       529,
			Keywords:        []string{"overloaded"},
			DurationMinutes: 5,
			RetryEnabled:    false, // 禁用重试
			Description:     "Overload rule (no retry)",
		},
	})

	gs := &GatewayService{}
	// getRetryRuleForError 应该返回 nil，因为规则禁用了重试
	rule, _ := gs.getRetryRuleForError(account, 529, []byte(`{"error": "server overloaded"}`))
	require.Nil(t, rule, "getRetryRuleForError should not return rules with retry disabled")

	// 但 getRetryRuleForErrorWithoutRetryCheck 应该返回规则（用于 failover）
	failoverRule, _ := gs.getRetryRuleForErrorWithoutRetryCheck(account, 529, []byte(`{"error": "server overloaded"}`))
	require.NotNil(t, failoverRule, "getRetryRuleForErrorWithoutRetryCheck should return the rule")
	require.Equal(t, 300, failoverRule.GetDurationSeconds())
}

// =============================================================================
// 测试用例：shouldRetryWithRule 逻辑
// =============================================================================

func TestShouldRetryWithRule(t *testing.T) {
	tests := []struct {
		name              string
		rules             []TempUnschedulableRule
		statusCode        int
		responseBody      string
		expectShouldRetry bool
		expectMaxAttempts int
		expectRuleNotNil  bool
	}{
		{
			name: "429 with retry enabled - should retry",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       429,
					Keywords:        []string{"rate limit"},
					DurationSeconds: 30,
					RetryEnabled:    true,
					RetryCount:      3,
				},
			},
			statusCode:        429,
			responseBody:      `{"error": "rate limit"}`,
			expectShouldRetry: true,
			expectMaxAttempts: 3,
			expectRuleNotNil:  true,
		},
		{
			name: "429 with retry disabled - should not retry via rule",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       429,
					Keywords:        []string{"rate limit"},
					DurationSeconds: 30,
					RetryEnabled:    false,
					RetryCount:      3,
				},
			},
			statusCode:        429,
			responseBody:      `{"error": "rate limit"}`,
			expectShouldRetry: false,
			expectMaxAttempts: 0,
			expectRuleNotNil:  false,
		},
		{
			name: "429 with max retry count (10)",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       429,
					Keywords:        []string{"rate limit"},
					DurationSeconds: 10,
					RetryEnabled:    true,
					RetryCount:      15, // will be capped to 10
				},
			},
			statusCode:        429,
			responseBody:      `{"error": "rate limit exceeded"}`,
			expectShouldRetry: true,
			expectMaxAttempts: 10, // capped
			expectRuleNotNil:  true,
		},
		{
			name: "no matching rule",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       429,
					Keywords:        []string{"rate limit"},
					DurationSeconds: 30,
					RetryEnabled:    true,
					RetryCount:      3,
				},
			},
			statusCode:        500,
			responseBody:      `{"error": "internal server error"}`,
			expectShouldRetry: false,
			expectMaxAttempts: 0,
			expectRuleNotNil:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := buildAccountWithRetryRules(tt.rules)
			gs := &GatewayService{}

			shouldRetry, maxAttempts, rule := gs.shouldRetryWithRule(account, tt.statusCode, []byte(tt.responseBody))

			require.Equal(t, tt.expectShouldRetry, shouldRetry, "shouldRetry mismatch")
			require.Equal(t, tt.expectMaxAttempts, maxAttempts, "maxAttempts mismatch")
			if tt.expectRuleNotNil {
				require.NotNil(t, rule, "expected rule to be returned")
			} else {
				require.Nil(t, rule, "expected no rule")
			}
		})
	}
}

// =============================================================================
// 测试用例：getRetryRuleForErrorWithoutRetryCheck (Failover场景)
// =============================================================================

func TestGetRetryRuleForErrorWithoutRetryCheck(t *testing.T) {
	account := buildAccountWithRetryRules([]TempUnschedulableRule{
		{
			ErrorCode:       429,
			Keywords:        []string{"rate limit"},
			DurationSeconds: 30,
			RetryEnabled:    false, // retry disabled
			RetryCount:      0,
		},
		{
			ErrorCode:       529,
			Keywords:        []string{"overloaded"},
			DurationMinutes: 2,
			RetryEnabled:    false,
		},
	})

	tests := []struct {
		name              string
		statusCode        int
		responseBody      string
		expectMatch       bool
		expectDurationSec int
	}{
		{
			name:              "429 rule found for failover (even without retry)",
			statusCode:        429,
			responseBody:      `{"error": "rate limit exceeded"}`,
			expectMatch:       true,
			expectDurationSec: 30,
		},
		{
			name:              "529 rule found for failover",
			statusCode:        529,
			responseBody:      `{"message": "service overloaded"}`,
			expectMatch:       true,
			expectDurationSec: 120, // 2 * 60
		},
		{
			name:         "no rule for 500",
			statusCode:   500,
			responseBody: `{"error": "server error"}`,
			expectMatch:  false,
		},
	}

	gs := &GatewayService{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, _ := gs.getRetryRuleForErrorWithoutRetryCheck(account, tt.statusCode, []byte(tt.responseBody))
			if tt.expectMatch {
				require.NotNil(t, rule)
				require.Equal(t, tt.expectDurationSec, rule.GetDurationSeconds())
			} else {
				require.Nil(t, rule)
			}
		})
	}
}

// =============================================================================
// 测试用例：Failover 场景下的限流时间设置
// =============================================================================

func TestFailoverScenarios(t *testing.T) {
	tests := []struct {
		name                     string
		rules                    []TempUnschedulableRule
		statusCode               int
		responseBody             string
		expectRateLimitDuration  int  // 期望的限流时间（秒），0表示无自定义限流
		expectFailover           bool // 是否应触发failover
	}{
		{
			name: "429 retry exhausted - use rule duration for rate limit",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       429,
					Keywords:        []string{"rate limit"},
					DurationSeconds: 45,
					RetryEnabled:    true,
					RetryCount:      3,
				},
			},
			statusCode:              429,
			responseBody:            `{"error": "rate limit exceeded"}`,
			expectRateLimitDuration: 45,
			expectFailover:          true,
		},
		{
			name: "429 no retry - direct failover with rule duration",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       429,
					Keywords:        []string{"rate limit"},
					DurationMinutes: 1,
					RetryEnabled:    false,
				},
			},
			statusCode:              429,
			responseBody:            `{"error": "rate limit hit"}`,
			expectRateLimitDuration: 60,
			expectFailover:          true,
		},
		{
			name: "529 overload - failover with minutes duration",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       529,
					Keywords:        []string{"overloaded"},
					DurationMinutes: 10,
					RetryEnabled:    false,
				},
			},
			statusCode:              529,
			responseBody:            `{"error": "service overloaded"}`,
			expectRateLimitDuration: 600, // 10 * 60
			expectFailover:          true,
		},
		{
			name: "mixed seconds and minutes - seconds takes priority",
			rules: []TempUnschedulableRule{
				{
					ErrorCode:       429,
					Keywords:        []string{"rate limit"},
					DurationMinutes: 5,
					DurationSeconds: 15, // takes priority
					RetryEnabled:    true,
					RetryCount:      2,
				},
			},
			statusCode:              429,
			responseBody:            `{"error": "rate limit"}`,
			expectRateLimitDuration: 15, // seconds takes priority
			expectFailover:          true,
		},
	}

	gs := &GatewayService{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := buildAccountWithRetryRules(tt.rules)

			// 获取用于failover的规则（不检查retry）
			rule, _ := gs.getRetryRuleForErrorWithoutRetryCheck(account, tt.statusCode, []byte(tt.responseBody))

			if tt.expectFailover && tt.expectRateLimitDuration > 0 {
				require.NotNil(t, rule, "expected rule for failover")
				require.Equal(t, tt.expectRateLimitDuration, rule.GetDurationSeconds(),
					"rate limit duration mismatch")
			}
		})
	}
}

// =============================================================================
// 测试用例：关键词匹配（大小写不敏感、部分匹配）
// =============================================================================

func TestKeywordMatching(t *testing.T) {
	account := buildAccountWithRetryRules([]TempUnschedulableRule{
		{
			ErrorCode:       429,
			Keywords:        []string{"Rate Limit", "TOO MANY"},
			DurationSeconds: 30,
			RetryEnabled:    true,
			RetryCount:      3,
		},
	})

	tests := []struct {
		name         string
		responseBody string
		expectMatch  bool
	}{
		{
			name:         "exact match lowercase",
			responseBody: `{"error": "rate limit exceeded"}`,
			expectMatch:  true,
		},
		{
			name:         "exact match uppercase",
			responseBody: `{"error": "RATE LIMIT EXCEEDED"}`,
			expectMatch:  true,
		},
		{
			name:         "mixed case match",
			responseBody: `{"error": "Rate Limit Exceeded"}`,
			expectMatch:  true,
		},
		{
			name:         "partial keyword match",
			responseBody: `{"message": "too many requests sent"}`,
			expectMatch:  true,
		},
		{
			name:         "no match",
			responseBody: `{"error": "unknown error occurred"}`,
			expectMatch:  false,
		},
		{
			name:         "keyword in nested field",
			responseBody: `{"error": {"type": "rate_limit_error", "message": "Rate limit exceeded"}}`,
			expectMatch:  true,
		},
	}

	gs := &GatewayService{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, _ := gs.getRetryRuleForError(account, 429, []byte(tt.responseBody))
			if tt.expectMatch {
				require.NotNil(t, rule, "expected rule to match")
			} else {
				require.Nil(t, rule, "expected no match")
			}
		})
	}
}

// =============================================================================
// 测试用例：边界条件
// =============================================================================

func TestEdgeCases(t *testing.T) {
	t.Run("nil account", func(t *testing.T) {
		gs := &GatewayService{}
		rule, keyword := gs.getRetryRuleForError(nil, 429, []byte(`{"error": "rate limit"}`))
		require.Nil(t, rule)
		require.Empty(t, keyword)
	})

	t.Run("temp_unschedulable disabled", func(t *testing.T) {
		account := &Account{
			ID: 1,
			Credentials: map[string]any{
				"temp_unschedulable_enabled": false,
				"temp_unschedulable_rules": []any{
					map[string]any{
						"error_code":       float64(429),
						"keywords":         []any{"rate limit"},
						"duration_seconds": float64(30),
						"retry_enabled":    true,
						"retry_count":      float64(3),
					},
				},
			},
		}
		gs := &GatewayService{}
		rule, _ := gs.getRetryRuleForError(account, 429, []byte(`{"error": "rate limit"}`))
		require.Nil(t, rule, "should not match when feature is disabled")
	})

	t.Run("empty rules", func(t *testing.T) {
		account := buildAccountWithRetryRules([]TempUnschedulableRule{})
		gs := &GatewayService{}
		rule, _ := gs.getRetryRuleForError(account, 429, []byte(`{"error": "rate limit"}`))
		require.Nil(t, rule)
	})

	t.Run("rule with zero duration is invalid", func(t *testing.T) {
		account := buildAccountWithRetryRules([]TempUnschedulableRule{
			{
				ErrorCode:       429,
				Keywords:        []string{"rate limit"},
				DurationMinutes: 0,
				DurationSeconds: 0, // invalid: no duration
				RetryEnabled:    true,
				RetryCount:      3,
			},
		})
		// 规则验证时会被过滤掉
		rules := account.GetTempUnschedulableRules()
		require.Len(t, rules, 0, "rule with zero duration should be filtered out")
	})

	t.Run("rule with empty keywords is invalid", func(t *testing.T) {
		account := buildAccountWithRetryRules([]TempUnschedulableRule{
			{
				ErrorCode:       429,
				Keywords:        []string{}, // invalid
				DurationSeconds: 30,
				RetryEnabled:    true,
				RetryCount:      3,
			},
		})
		rules := account.GetTempUnschedulableRules()
		require.Len(t, rules, 0, "rule with empty keywords should be filtered out")
	})
}

// =============================================================================
// 测试用例：完整的重试流程模拟
// =============================================================================

func TestRetryFlowSimulation(t *testing.T) {
	t.Run("simulate retry attempts exhaustion", func(t *testing.T) {
		account := buildAccountWithRetryRules([]TempUnschedulableRule{
			{
				ErrorCode:       429,
				Keywords:        []string{"rate limit"},
				DurationSeconds: 30,
				RetryEnabled:    true,
				RetryCount:      3,
			},
		})

		gs := &GatewayService{}
		responseBody := []byte(`{"error": {"type": "rate_limit_error", "message": "rate limit exceeded"}}`)

		// 模拟重试流程
		shouldRetry, maxAttempts, rule := gs.shouldRetryWithRule(account, 429, responseBody)

		require.True(t, shouldRetry, "first attempt should trigger retry")
		require.Equal(t, 3, maxAttempts, "max attempts should be 3")
		require.NotNil(t, rule)

		// 模拟重试耗尽后获取failover规则
		failoverRule, _ := gs.getRetryRuleForErrorWithoutRetryCheck(account, 429, responseBody)
		require.NotNil(t, failoverRule)
		require.Equal(t, 30, failoverRule.GetDurationSeconds(),
			"failover should use rule's duration for rate limiting")
	})

	t.Run("simulate no retry rule - direct failover", func(t *testing.T) {
		account := buildAccountWithRetryRules([]TempUnschedulableRule{
			{
				ErrorCode:       429,
				Keywords:        []string{"rate limit"},
				DurationMinutes: 2,
				RetryEnabled:    false, // no retry
			},
		})

		gs := &GatewayService{}
		responseBody := []byte(`{"error": "rate limit exceeded"}`)

		// 检查是否应该重试 - 不应该
		shouldRetry, _, rule := gs.shouldRetryWithRule(account, 429, responseBody)
		require.False(t, shouldRetry, "should not retry when retry is disabled")
		require.Nil(t, rule)

		// 但是 failover 时应该能获取到规则
		failoverRule, _ := gs.getRetryRuleForErrorWithoutRetryCheck(account, 429, responseBody)
		require.NotNil(t, failoverRule, "should get rule for failover duration")
		require.Equal(t, 120, failoverRule.GetDurationSeconds(), "should use 2 minutes = 120 seconds")
	})
}
