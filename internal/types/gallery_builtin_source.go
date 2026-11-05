package types

// builtin 属性来源把每个画廊都具备的图片字段——图片描述、OCR 文本、时间戳以及
// chunk 启停标志——建模成属性，使画廊的筛选 / 搜索 / 排序界面从一份统一契约
// 渲染，没有硬编码的特殊分支。builtin 属性的取值不存放在 image_info.attrs 中；
// service 层从资产自身的字段解析它们（见 service 包的 galleryAttrs 辅助函数）。
//
// 默认值有意做到开箱即用：caption 和 ocr_text 可搜索，时间戳可排序，启停标志
// 可筛选。每个标志都可在运行时按层（系统 / 知识库 / 用户）覆盖，无需改动本文件。
//
// 注：Label/Description 为契约下发的英文兜底文案，界面实际显示由前端 i18n
// （gallery.attr.builtin_*）本地化。
func init() {
	RegisterGalleryAttrSource("builtin", func(string) []GalleryAttrDef {
		return []GalleryAttrDef{
			{
				Name:        "caption",
				Type:        "text",
				Label:       "Caption",
				Description: "The model-generated description of the image.",
				Usage:       GalleryUsage{InSearchField: true, InSortField: true},
			},
			{
				Name:        "ocr_text",
				Type:        "text",
				Label:       "OCR text",
				Description: "Text extracted from the image by OCR.",
				Usage:       GalleryUsage{InSearchField: true},
			},
			{
				Name:  "created_at",
				Type:  "date",
				Label: "Created at",
				Usage: GalleryUsage{InSortField: true},
			},
			{
				Name:  "updated_at",
				Type:  "date",
				Label: "Updated at",
				Usage: GalleryUsage{InSortField: true},
			},
			{
				Name:  "is_enabled",
				Type:  "presence",
				Label: "Enabled",
				Values: []GalleryAttrValue{
					{Value: "true", Label: "Enabled"},
					{Value: "false", Label: "Disabled"},
				},
				Usage: GalleryUsage{InFilter: true},
			},
		}
	})
}
