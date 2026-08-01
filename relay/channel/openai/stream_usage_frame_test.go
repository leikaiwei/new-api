package openai

import (
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 取自生产实际流量（opencode zen/go 端点，deepseek-v4-flash）。
// 该上游在带 usage 的 finish_reason 帧之后还会追加一个自有成本帧，
// 只解析流的最后一帧会把真实 usage 丢掉并回退本地估算。
const (
	realUsageFrame = `{"id":"c07fd9d0-81f2-45b6-ad13-eead550420d5","object":"chat.completion.chunk","created":1785594120,"model":"deepseek-v4-flash","system_fingerprint":"fp_a18b46594c_prod0820_fp8_kvcache_20260402","choices":[{"index":0,"delta":{"content":"","reasoning_content":null},"logprobs":null,"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":31143,"completion_tokens":1820,"total_tokens":32963,"prompt_tokens_details":{"cached_tokens":29568},"completion_tokens_details":{"reasoning_tokens":1017},"prompt_cache_hit_tokens":29568,"prompt_cache_miss_tokens":1575}}`

	realCostFrame = `{"choices":[],"x-opencode-type":"inference-cost","cost":"0.00063346","normalizedUsage":{"inputTokens":2813,"outputTokens":436,"reasoningTokens":0,"cacheReadTokens":41984,"cacheWrite5mTokens":0,"cacheWrite1hTokens":0}}`
)

func TestStreamDataHasBillableUsage(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "上游真实 usage 帧",
			data: realUsageFrame,
			want: true,
		},
		{
			name: "上游追加的成本元数据帧不含可计费 usage",
			data: realCostFrame,
			want: false,
		},
		{
			name: "普通增量帧",
			data: `{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
			want: false,
		},
		{
			name: "usage 为 null",
			data: `{"choices":[{"index":0,"delta":{}}],"usage":null}`,
			want: false,
		},
		{
			name: "usage 全零视为无效",
			data: `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
			want: false,
		},
		{
			name: "只有 completion_tokens 也算有效",
			data: `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":7,"total_tokens":7}}`,
			want: true,
		},
		{
			name: "非法 JSON",
			data: `{"usage":`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, streamDataHasBillableUsage(tc.data))
		})
	}
}

// 成本帧喂给 handleLastResponse 时既拿不到 usage，也会把响应元数据清空，
// 这正是记账归零的根因；换成记录下来的 usage 帧则应完整恢复。
func TestHandleLastResponseUsageRecovery(t *testing.T) {
	call := func(data string) (usage *dto.Usage, contains bool, responseId string, model string, fingerprint string) {
		var createAt int64
		usage = &dto.Usage{}
		shouldSend := true
		info := &relaycommon.RelayInfo{}
		err := handleLastResponse(data, &responseId, &createAt, &fingerprint, &model, &usage,
			&contains, info, &shouldSend)
		require.NoError(t, err)
		return usage, contains, responseId, model, fingerprint
	}

	t.Run("成本帧不携带 usage 且清空元数据", func(t *testing.T) {
		usage, contains, responseId, model, fingerprint := call(realCostFrame)

		assert.False(t, contains, "成本帧不应被判定为携带上游 usage")
		assert.Zero(t, usage.PromptTokens)
		assert.Zero(t, usage.CompletionTokens)
		assert.Empty(t, responseId)
		assert.Empty(t, model)
		assert.Empty(t, fingerprint)
	})

	t.Run("usage 帧恢复真实用量与元数据", func(t *testing.T) {
		usage, contains, responseId, model, fingerprint := call(realUsageFrame)

		require.True(t, contains, "usage 帧应被判定为携带上游 usage")
		assert.Equal(t, 31143, usage.PromptTokens)
		assert.Equal(t, 1820, usage.CompletionTokens)
		assert.Equal(t, 32963, usage.TotalTokens)
		assert.Equal(t, 29568, usage.PromptTokensDetails.CachedTokens)
		assert.Equal(t, "c07fd9d0-81f2-45b6-ad13-eead550420d5", responseId)
		assert.Equal(t, "deepseek-v4-flash", model)
		assert.Equal(t, "fp_a18b46594c_prod0820_fp8_kvcache_20260402", fingerprint)
	})
}
