package types

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
)

// 原文定位类型定义。
// 定位器（locator）把 chunk 指回用户上传的原始文件，使引用可以直接打开文件并
// 定位到被引用的位置。所有序号（页码、幻灯片、块、章节、行）均从 1 开始。
const (
	// SourceLocatorPDF 表示 PDF 页面，可进一步收窄到页面中的区域。
	SourceLocatorPDF = "pdf"
	// SourceLocatorDocx 表示 Word 文件中第 N 个正文级段落或表格。
	SourceLocatorDocx = "docx"
	// SourceLocatorSlide 表示演示文稿中的幻灯片。
	SourceLocatorSlide = "slide"
	// SourceLocatorSheet 表示电子表格某个工作表中的行范围。
	SourceLocatorSheet = "sheet"
	// SourceLocatorText 表示纯文本原文（md、txt、json）中的字符范围。
	SourceLocatorText = "text"
	// SourceLocatorTime 表示音频原文中的时间区间。
	SourceLocatorTime = "time"
	// SourceLocatorSection 表示 EPUB 的一个 spine 项。
	SourceLocatorSection = "section"
)

// SourceLocatorQuoteMax 限制展示用的引用片段长度，不影响存储的原文证据。
const SourceLocatorQuoteMax = 300

// SourceLocator 表示原始文件中的一个位置。
// 只有其 Type 对应的字段会被赋值；零值不会出现在 JSON 中，读回时为零值。
type SourceLocator struct {
	Type string `json:"type"`
	// Mapping 记录已验证的文本与原文对齐关系；为空表示旧版数据。
	Mapping string `json:"mapping,omitempty"`
	// Partial 表示 chunk 中有一部分内容没有原文映射。
	Partial bool `json:"partial,omitempty"`
	// SourceID 标识解析结果中的块，而不是渲染后的条目。
	SourceID string `json:"source_id,omitempty"`
	// SourceHash 把定位器绑定到知识记录当时的原文版本哈希。
	SourceHash string `json:"source_hash,omitempty"`
	// Page 是 PDF 页码，从 1 开始。
	Page int `json:"page,omitempty"`
	// BBox 为 [x0, y0, x1, y1]，取页面渲染后宽高的比例值，
	// 原点位于渲染页面的左上角。
	BBox []float64 `json:"bbox,omitempty"`
	// Block 是 Word 文件正文段落和表格中的 1 基序号。
	Block int `json:"block,omitempty"`
	// Slide 是演示文稿按放映顺序的 1 基幻灯片编号。
	Slide int `json:"slide,omitempty"`
	// Sheet 是电子表格的工作表名称；RowStart/RowEnd 是表格软件中显示的
	// 1 基行号。
	Sheet    string `json:"sheet,omitempty"`
	RowStart int    `json:"row_start,omitempty"`
	RowEnd   int    `json:"row_end,omitempty"`
	// Start/End 是纯文本原始文件中的字符偏移量。
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`
	// StartMs/EndMs 界定一段音频区间。
	StartMs int64 `json:"start_ms,omitempty"`
	EndMs   int64 `json:"end_ms,omitempty"`
	// Section 是 EPUB spine 的 1 基索引。
	Section int `json:"section,omitempty"`
	// Title 在原文有标题时记录章节或幻灯片标题。
	Title string `json:"title,omitempty"`
	// Quote 是按解析结果得到的完整引用重叠文本。查看器会在渲染后的原文中
	// 匹配它，也会把它和回答中引用该 chunk 的句子进行匹配。
	Quote string `json:"quote,omitempty"`
}

// SourceBlock 把解析器 Markdown 输出中的一段字符范围，关联到它在原始文件中
// 的来源位置。
type SourceBlock struct {
	Start   int
	End     int
	Locator SourceLocator
}

// SourceLocators 是每个 chunk 的定位器列表，以 JSON 列存储。
type SourceLocators []SourceLocator

// Scan 实现 sql.Scanner 接口。
func (s *SourceLocators) Scan(value interface{}) error {
	if value == nil {
		*s = nil
		return nil
	}
	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return errors.New("SourceLocators: 类型断言为 []byte 或 string 失败")
	}
	if len(raw) == 0 || string(raw) == "null" {
		*s = nil
		return nil
	}
	return json.Unmarshal(raw, s)
}

// Value 实现 driver.Valuer 接口。空列表存储为 NULL。
func (s SourceLocators) Value() (driver.Value, error) {
	if len(s) == 0 {
		return nil, nil
	}
	b, err := json.Marshal([]SourceLocator(s))
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
