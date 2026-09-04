package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 入口估算与 Claude→OpenAI 转换必须口径一致：tool_result 里的 image 块按图片计，
// 不再把几十万字符的 base64 当文本估（那会让 message_start 的 input_tokens 只有真值一半）。
func TestClaudeRequestTokenCountMetaToolResultImage(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

	t.Run("带图片的 tool_result：图片进 Files，文本进 CombineText，base64 不进文本", func(t *testing.T) {
		req := ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
				map[string]any{"type": "text", "text": "screenshot taken"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
			}},
		}}}}
		meta := req.GetTokenCountMeta()
		require.Len(t, meta.Files, 1)
		assert.Equal(t, types.FileTypeImage, meta.Files[0].FileType)
		assert.Contains(t, meta.CombineText, "screenshot taken")
		assert.NotContains(t, meta.CombineText, png)
	})

	t.Run("不带图片的 tool_result 保持整体序列化", func(t *testing.T) {
		req := ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
				map[string]any{"type": "text", "text": "line one"},
			}},
		}}}}
		meta := req.GetTokenCountMeta()
		assert.Empty(t, meta.Files)
		assert.Contains(t, meta.CombineText, `"type":"text"`)
		assert.Contains(t, meta.CombineText, "line one")
	})
}
