//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTempUnschedulableRule_GetDurationSeconds(t *testing.T) {
	tests := []struct {
		name            string
		durationMinutes int
		durationSeconds int
		expected        int
	}{
		{
			name:            "only duration_minutes set",
			durationMinutes: 5,
			durationSeconds: 0,
			expected:        300, // 5 * 60
		},
		{
			name:            "only duration_seconds set",
			durationMinutes: 0,
			durationSeconds: 30,
			expected:        30,
		},
		{
			name:            "both set, duration_seconds takes priority",
			durationMinutes: 5,
			durationSeconds: 45,
			expected:        45,
		},
		{
			name:            "both zero",
			durationMinutes: 0,
			durationSeconds: 0,
			expected:        0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := &TempUnschedulableRule{
				DurationMinutes: tt.durationMinutes,
				DurationSeconds: tt.durationSeconds,
			}
			require.Equal(t, tt.expected, rule.GetDurationSeconds())
		})
	}
}

func TestTempUnschedulableRule_GetRetryCount(t *testing.T) {
	tests := []struct {
		name         string
		retryEnabled bool
		retryCount   int
		expected     int
	}{
		{
			name:         "retry disabled",
			retryEnabled: false,
			retryCount:   5,
			expected:     0,
		},
		{
			name:         "retry enabled with valid count",
			retryEnabled: true,
			retryCount:   3,
			expected:     3,
		},
		{
			name:         "retry enabled with zero count",
			retryEnabled: true,
			retryCount:   0,
			expected:     0,
		},
		{
			name:         "retry enabled with negative count",
			retryEnabled: true,
			retryCount:   -1,
			expected:     0,
		},
		{
			name:         "retry enabled with count over 10",
			retryEnabled: true,
			retryCount:   15,
			expected:     10, // capped at 10
		},
		{
			name:         "retry enabled with count at 10",
			retryEnabled: true,
			retryCount:   10,
			expected:     10,
		},
		{
			name:         "retry enabled with count at 1",
			retryEnabled: true,
			retryCount:   1,
			expected:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := &TempUnschedulableRule{
				RetryEnabled: tt.retryEnabled,
				RetryCount:   tt.retryCount,
			}
			require.Equal(t, tt.expected, rule.GetRetryCount())
		})
	}
}

func TestAccount_GetTempUnschedulableRulesWithRetry(t *testing.T) {
	account := &Account{
		Credentials: map[string]any{
			"temp_unschedulable_enabled": true,
			"temp_unschedulable_rules": []any{
				map[string]any{
					"error_code":       float64(429),
					"keywords":         []any{"rate limit"},
					"duration_seconds": float64(30),
					"retry_enabled":    true,
					"retry_count":      float64(3),
				},
				map[string]any{
					"error_code":       float64(529),
					"keywords":         []any{"overloaded"},
					"duration_minutes": float64(5),
					"retry_enabled":    false,
				},
			},
		},
	}

	require.True(t, account.IsTempUnschedulableEnabled())

	rules := account.GetTempUnschedulableRules()
	require.Len(t, rules, 2)

	// First rule: 429 with retry
	require.Equal(t, 429, rules[0].ErrorCode)
	require.Equal(t, []string{"rate limit"}, rules[0].Keywords)
	require.Equal(t, 30, rules[0].GetDurationSeconds())
	require.True(t, rules[0].RetryEnabled)
	require.Equal(t, 3, rules[0].GetRetryCount())

	// Second rule: 529 without retry
	require.Equal(t, 529, rules[1].ErrorCode)
	require.Equal(t, []string{"overloaded"}, rules[1].Keywords)
	require.Equal(t, 300, rules[1].GetDurationSeconds()) // 5 * 60
	require.False(t, rules[1].RetryEnabled)
	require.Equal(t, 0, rules[1].GetRetryCount())
}
