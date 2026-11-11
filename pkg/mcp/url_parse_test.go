package mcp

import (
	"net/url"
	"strings"
	"testing"
)

// TestURLParseError 复现URL解析错误
func TestURLParseError(t *testing.T) {
	tests := []struct {
		name     string
		rawURL   string
		wantErr  bool
		errMsg   string
	}{
		{
			name:    "正确的URL",
			rawURL:  "https://mcagent.100.me/api/v1/mcp/sse",
			wantErr: false,
		},
		{
			name:    "包含反引号的URL",
			rawURL:  "`https://mcagent.100.me/api/v1/mcp/sse`",
			wantErr: true,
			errMsg:  "first path segment in URL cannot contain colon",
		},
		{
			name:    "包含空格和反引号的URL",
			rawURL:  " `https://mcagent.100.me/api/v1/mcp/sse` ",
			wantErr: true,
			errMsg:  "first path segment in URL cannot contain colon",
		},
		{
			name:    "前后有空格的URL",
			rawURL:  " https://mcagent.100.me/api/v1/mcp/sse ",
			wantErr: true,
			errMsg:  "first path segment in URL cannot contain colon",
		},
		{
			name:    "双引号包裹的URL",
			rawURL:  "\"https://mcagent.100.me/api/v1/mcp/sse\"",
			wantErr: true,
			errMsg:  "first path segment in URL cannot contain colon",
		},
		{
			name:    "单引号包裹的URL",
			rawURL:  "'https://mcagent.100.me/api/v1/mcp/sse'",
			wantErr: true,
			errMsg:  "first path segment in URL cannot contain colon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := url.Parse(tt.rawURL)
			
			if tt.wantErr {
				if err == nil {
					t.Errorf("期望解析失败，但成功了: %s", tt.rawURL)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("错误信息不匹配: got %v, want contains %s", err, tt.errMsg)
				} else {
					t.Logf("✓ 正确捕获错误: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("期望解析成功，但失败: %v", err)
				} else {
					t.Logf("✓ 解析成功: %s", tt.rawURL)
				}
			}
		})
	}
}

// TestURLCleanup 测试URL清理函数
func TestURLCleanup(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "正确的URL",
			input:    "https://mcagent.100.me/api/v1/mcp/sse",
			expected: "https://mcagent.100.me/api/v1/mcp/sse",
		},
		{
			name:     "包含反引号",
			input:    "`https://mcagent.100.me/api/v1/mcp/sse`",
			expected: "https://mcagent.100.me/api/v1/mcp/sse",
		},
		{
			name:     "包含空格和反引号",
			input:    " `https://mcagent.100.me/api/v1/mcp/sse` ",
			expected: "https://mcagent.100.me/api/v1/mcp/sse",
		},
		{
			name:     "双引号包裹",
			input:    "\"https://mcagent.100.me/api/v1/mcp/sse\"",
			expected: "https://mcagent.100.me/api/v1/mcp/sse",
		},
		{
			name:     "单引号包裹",
			input:    "'https://mcagent.100.me/api/v1/mcp/sse'",
			expected: "https://mcagent.100.me/api/v1/mcp/sse",
		},
		{
			name:     "多对引号",
			input:    "`'\"https://mcagent.100.me/api/v1/mcp/sse\"'`",
			expected: "https://mcagent.100.me/api/v1/mcp/sse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned := cleanURL(tt.input)
			
			if cleaned != tt.expected {
				t.Errorf("清理失败: got %q, want %q", cleaned, tt.expected)
			} else {
				// 验证清理后的URL可以被正确解析
				_, err := url.Parse(cleaned)
				if err != nil {
					t.Errorf("清理后的URL仍然无法解析: %v", err)
				} else {
					t.Logf("✓ 清理成功: %q -> %q", tt.input, cleaned)
				}
			}
		})
	}
}

// cleanURL 清理URL字符串中的多余字符
func cleanURL(rawURL string) string {
	// 1. 去除前后空格
	s := strings.TrimSpace(rawURL)
	
	// 2. 去除引号（单引号、双引号、反引号）
	for len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') ||
		   (s[0] == '\'' && s[len(s)-1] == '\'') ||
		   (s[0] == '`' && s[len(s)-1] == '`') {
			s = s[1:len(s)-1]
		} else {
			break
		}
	}
	
	// 3. 再次去除空格（引号内可能还有空格）
	s = strings.TrimSpace(s)
	
	return s
}

// TestURLCleanAndParse 综合测试：清理后解析
func TestURLCleanAndParse(t *testing.T) {
	testCases := []string{
		"https://mcagent.100.me/api/v1/mcp/sse",
		"`https://mcagent.100.me/api/v1/mcp/sse`",
		" `https://mcagent.100.me/api/v1/mcp/sse` ",
		"\"https://mcagent.100.me/api/v1/mcp/sse\"",
		"'https://mcagent.100.me/api/v1/mcp/sse'",
	}

	for i, rawURL := range testCases {
		t.Run("", func(t *testing.T) {
			// 清理URL
			cleaned := cleanURL(rawURL)
			
			// 解析URL
			parsed, err := url.Parse(cleaned)
			if err != nil {
				t.Errorf("Case %d: 解析失败: %v (raw=%q, cleaned=%q)", i, err, rawURL, cleaned)
				return
			}
			
			// 验证解析结果
			if parsed.Scheme != "https" {
				t.Errorf("Case %d: 协议错误: got %s, want https", i, parsed.Scheme)
			}
			if parsed.Host != "mcagent.100.me" {
				t.Errorf("Case %d: 主机错误: got %s, want mcagent.100.me", i, parsed.Host)
			}
			if parsed.Path != "/api/v1/mcp/sse" {
				t.Errorf("Case %d: 路径错误: got %s, want /api/v1/mcp/sse", i, parsed.Path)
			}
			
			t.Logf("✓ Case %d: 成功 (raw=%q -> cleaned=%q -> parsed=%s)", i, rawURL, cleaned, parsed)
		})
	}
}