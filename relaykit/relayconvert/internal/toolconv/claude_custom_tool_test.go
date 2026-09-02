package toolconv

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Anthropic 的自定义工具可以显式带 type:"custom"，语义等同于不带 type。
// 若按通用 native-type 表归为 KindNative，encode 时会整份丢弃，
// Agent 类客户端（Claude Code、Codely CLI）会拿不到任何工具。
func TestDecodeClaudeDefinitionCustomTypeIsFunction(t *testing.T) {
	cases := []struct {
		name           string
		raw            string
		wantNativeType string
	}{
		{
			name:           "省略 type（Anthropic 简写形状）",
			raw:            `{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}`,
			wantNativeType: "",
		},
		{
			name:           "显式 type=custom（Anthropic 完整形状）",
			raw:            `{"type":"custom","name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}`,
			wantNativeType: "custom",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			definition, err := decodeClaudeDefinition(json.RawMessage(tc.raw))
			require.NoError(t, err)

			assert.Equal(t, KindFunction, definition.Kind)
			assert.Equal(t, ExecutionClient, definition.Execution)
			assert.Equal(t, tc.wantNativeType, definition.NativeType)
			assert.Equal(t, "Read", definition.Name)

			require.NotNil(t, definition.Function, "自定义工具必须解出 Function，否则各 encode 目标会整份丢弃它")
			assert.Equal(t, "Read", definition.Function.Name)
			assert.Equal(t, "read a file", definition.Function.Description)
			assert.NotNil(t, definition.Function.Parameters)
		})
	}
}

// hosted tool 不能被这条放行规则误伤：它们仍须归为对应的非 Function Kind。
func TestDecodeClaudeDefinitionHostedToolsUnaffected(t *testing.T) {
	cases := []struct {
		raw      string
		wantKind Kind
	}{
		{`{"type":"computer_20241022","name":"computer"}`, KindComputerUse},
		{`{"type":"code_execution_20250522","name":"code_execution"}`, KindCodeExecution},
		{`{"type":"mcp","name":"mcp"}`, KindMCP},
	}

	for _, tc := range cases {
		definition, err := decodeClaudeDefinition(json.RawMessage(tc.raw))
		require.NoError(t, err)
		assert.Equal(t, tc.wantKind, definition.Kind)
		assert.Nil(t, definition.Function)
	}
}
