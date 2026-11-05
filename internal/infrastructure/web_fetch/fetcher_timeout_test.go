package web_fetch

import (
	"testing"
	"time"
)

// TestPageFetchTimeout 校验 WEKNORA_WEB_FETCH_TIMEOUT 的解析规则：
// Go duration、纯数字秒数均可；未设置/空白/非法/非正值回落到默认预算。
func TestPageFetchTimeout(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want time.Duration
	}{
		{"未设置", false, "", defaultPageFetchTimeout},
		{"空白", true, "   ", defaultPageFetchTimeout},
		{"标准 duration", true, "150s", 150 * time.Second},
		{"分钟单位", true, "3m", 3 * time.Minute},
		{"纯数字按秒", true, "180", 180 * time.Second},
		{"非法字符串", true, "abc", defaultPageFetchTimeout},
		{"零值", true, "0", defaultPageFetchTimeout},
		{"负值", true, "-5s", defaultPageFetchTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("WEKNORA_WEB_FETCH_TIMEOUT", tc.val)
			} else {
				// t.Setenv 无法在用例内“取消设置”，该分支由其他用例
				// 不碰该变量 + 下面的默认值断言共同覆盖。
				t.Setenv("WEKNORA_WEB_FETCH_TIMEOUT", "")
			}
			if got := PageFetchTimeout(); got != tc.want {
				t.Fatalf("PageFetchTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}
