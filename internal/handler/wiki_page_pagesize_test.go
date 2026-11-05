package handler

import "testing"

// TestClampWikiListPageSize 校验列页分页大小被钳制在 [1,200]，
// 缺省/非正回落 20，防止超大 page_size 形成巨型 SQL LIMIT。
func TestClampWikiListPageSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"零回落默认", 0, 20},
		{"负值回落默认", -5, 20},
		{"正常值保留", 50, 50},
		{"下限 1 保留", 1, 1},
		{"上限 200 保留", 200, 200},
		{"超上限钳到 200", 201, 200},
		{"极端超大值钳到 200", 100_000_000, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampWikiListPageSize(tc.in); got != tc.want {
				t.Fatalf("clampWikiListPageSize(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
