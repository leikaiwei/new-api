package relayconvert

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Anthropic 的自定义工具可以显式带 type:"custom"（区别于 computer/bash 这类 hosted tool），
// 而 OpenAI Responses 的 custom 是语义相反的特殊工具。通用 native-type 表按后者把 Claude 的
// custom 归为 hosted，转换时整份客户端工具被丢弃，Agent 类客户端拿不到任何工具、对话空转。
// 这里走生产入口 ConvertRequest，覆盖 tools 经 ExtractRequest/AttachRequest 的真实路径。
func TestConvertRequestClaudeCustomTypedToolsSurvive(t *testing.T) {
	tests := []struct {
		name   string
		target types.RelayFormat
		tools  string
	}{
		{
			name:   "Claude→OpenAI Chat，工具显式带 type=custom",
			target: types.RelayFormatOpenAI,
			tools:  `[{"type":"custom","name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]`,
		},
		{
			name:   "Claude→OpenAI Chat，工具省略 type（对照）",
			target: types.RelayFormatOpenAI,
			tools:  `[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &dto.ClaudeRequest{
				Model:    "claude-test",
				Messages: []dto.ClaudeMessage{{Role: "user", Content: mustRawMessage(t, []map[string]any{{"type": "text", "text": "list the files"}})}},
				Tools:    mustRawMessage(t, rawJSON(tc.tools)),
			}

			result, err := ConvertRequest(nil, &convmeta.Values{}, tc.target, req)
			require.NoError(t, err)

			openAIRequest, ok := result.Value.(*dto.GeneralOpenAIRequest)
			require.True(t, ok, "期望 OpenAI Chat 请求，得到 %T", result.Value)

			require.Len(t, openAIRequest.Tools, 1, "客户端自定义工具必须保留，不能被当成不可表示的 hosted tool 丢弃")
			assert.Equal(t, "function", openAIRequest.Tools[0].Type)
			assert.Equal(t, "Read", openAIRequest.Tools[0].Function.Name)
			assert.Equal(t, "read a file", openAIRequest.Tools[0].Function.Description)
			assert.NotNil(t, openAIRequest.Tools[0].Function.Parameters)

			for _, diagnostic := range result.Diagnostics {
				assert.NotEqual(t, "unsupported_hosted_tool", diagnostic.Code,
					"自定义工具不应产生 hosted-tool 丢失诊断：%s", diagnostic.Message)
			}
		})
	}
}
