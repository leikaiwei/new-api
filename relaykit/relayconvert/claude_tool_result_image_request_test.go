package relayconvert

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Claude Code 截图类工具的 tool_result 携带 image 块，Claude→OpenAI Chat 转换原先把整个块数组
// JSON 序列化进 tool 消息，几十万字符的 base64 被上游按文本计 token，几轮就撞满 1M 窗口。
// 这里走生产入口 ConvertRequest，锁定：tool 消息不含 base64，图片以 image_url 落在紧随其后的
// user 消息里，且 assistant(tool_calls) → tool → user 的顺序满足 OpenAI 对工具回复相邻的要求。
func TestConvertRequestClaudeToolResultImageBecomesImageURL(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	req := &dto.ClaudeRequest{
		Model: "mimo-v2.5",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: mustRawMessage(t, []map[string]any{{"type": "text", "text": "take a screenshot"}})},
			{Role: "assistant", Content: mustRawMessage(t, []map[string]any{
				{"type": "tool_use", "id": "toolu_1", "name": "screenshot", "input": map[string]any{}},
			})},
			{Role: "user", Content: mustRawMessage(t, []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": []map[string]any{
					{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
				}},
			})},
		},
		Tools: mustRawMessage(t, rawJSON(`[{"name":"screenshot","description":"take a screenshot","input_schema":{"type":"object","properties":{}}}]`)),
	}

	result, err := ConvertRequest(nil, &convmeta.Values{}, types.RelayFormatOpenAI, req)
	require.NoError(t, err)
	openAIRequest, ok := result.Value.(*dto.GeneralOpenAIRequest)
	require.True(t, ok, "期望 OpenAI Chat 请求，得到 %T", result.Value)

	require.Len(t, openAIRequest.Messages, 4)
	assert.Equal(t, "user", openAIRequest.Messages[0].Role)
	assert.Equal(t, "assistant", openAIRequest.Messages[1].Role)
	require.NotEmpty(t, openAIRequest.Messages[1].ToolCalls, "assistant 的 tool_calls 必须保留")
	assert.Equal(t, "tool", openAIRequest.Messages[2].Role)
	assert.Equal(t, "toolu_1", openAIRequest.Messages[2].ToolCallId)
	assert.NotContains(t, openAIRequest.Messages[2].StringContent(), png, "base64 不得以文本形式留在 tool 消息里")
	assert.Equal(t, "user", openAIRequest.Messages[3].Role)

	parts := openAIRequest.Messages[3].ParseContent()
	require.Len(t, parts, 2)
	assert.Equal(t, "image_url", parts[1].Type)
	media := parts[1].GetImageMedia()
	require.NotNil(t, media)
	assert.Equal(t, "data:image/png;base64,"+png, media.Url)

	// 出站 JSON 里 base64 只能出现在 image_url 这一处
	body, err := kitutil.Marshal(openAIRequest)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(body), png), "base64 只能出现在 image_url 里一次")
	assert.NotContains(t, string(body), `\"type\":\"image\"`, "不得再有被序列化成文本的图片块")
}
