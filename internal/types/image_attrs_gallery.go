package types

// 系统属性注册表同时充当画廊的一个属性来源：为「观察并描述」管线声明的每个属性
// 都提供给图片画廊，默认可筛选（观察值是小型有序集合，正是画廊 extent /
// presence 筛选最擅长渲染的形态）。画廊的配置分层可在运行时覆盖这些标记 ——
// 本注册只是最低优先级的兜底。
//
// 本文件依赖画廊的中性来源钩子（types/gallery_attr_source.go）。先合并画廊 PR，
// 或把钩子文件与本提交一同落地 —— 两个 PR 携带该文件逐字节相同的副本，因此任一
// 合并顺序都能干净解决。
func init() {
	RegisterGalleryAttrSource("system", func(string) []GalleryAttrDef {
		defs := make([]GalleryAttrDef, 0, len(ImageAttrRegistry))
		for _, spec := range ImageAttrRegistry {
			values := make([]GalleryAttrValue, 0, len(spec.Values))
			for _, value := range spec.Values {
				values = append(values, GalleryAttrValue(value))
			}
			defs = append(defs, GalleryAttrDef{
				Name:        spec.Name,
				Type:        string(spec.Type),
				Values:      values,
				Label:       spec.Label,
				Description: spec.Description,
				// extent / presence 观察默认为筛选素材；对它们搜索与排序是
				// 运营经画廊策略分层做出的决定。
				Usage: GalleryUsage{InFilter: true},
			})
		}
		return defs
	})
}
