package types

import (
	"regexp"
	"sync"
)

// GalleryUsage 声明图片画廊可以如何使用某个属性。它是“资格”层：一个属性只会
// 出现在其 usage 标志允许的画廊界面区块里（筛选面板 / 搜索字段复选框 / 排序
// 下拉）。某个搜索字段是否真正处于激活状态，由用户层的 status 单独决定，绝不
// 在这里决定。
type GalleryUsage struct {
	// InFilter 标记该属性可在筛选面板中渲染。
	InFilter bool `json:"in_filter"`
	// InSearchField 标记该属性可在搜索字段复选框中被选择
	// （用户仍会逐字段选择是否对其搜索）。
	InSearchField bool `json:"in_searchfield"`
	// InSortField 标记该属性可作为排序下拉中的一个选项。
	InSortField bool `json:"in_sortfield"`
}

// GalleryAttrValue 表示某个属性的一个允许值，以及要为它展示的文案。一个值有意
// 携带两段文本：Label 是上屏的短名（筛选复选框、搜索字段、排序选项），而
// Description 保存较长的解释，若内联展示会撑爆紧凑控件。调用方按需取用——
// 筛选面板渲染 Label 并把 Description 放进 tooltip，详情面板则可两者都显示。
//
// Value 始终是机器原始值：它是被筛选、搜索并存入 image_info 的内容，绝不允许
// 本地化。
type GalleryAttrValue struct {
	// Value 为该属性可取的原始字符串。
	Value string `json:"value"`
	// Label 为该值上屏用的短名。
	Label string `json:"label"`
	// Description 用一句话解释该值；来源没有更多内容时为空。
	Description string `json:"description,omitempty"`
}

// GalleryAttrDef 表示某个来源声明的、可供画廊消费的一个属性。Name 是来源内的
// （不带前缀）；画廊在构建契约时以声明来源的 id 为其加命名空间（见
// GalleryAttrID）。Type 驱动前端渲染：“text”（子串搜索）、“date”、
// “extent”（有序取值集 → 多选）、“presence”（布尔 → true/false 复选框）
// 以及 “keywords”（标签数组 → 标签搜索）。
//
// 与值类似，属性本身也带有给界面用的短 Label，和在放得下一句话之处使用的较长
// Description。
type GalleryAttrDef struct {
	Name        string             `json:"name"`
	Type        string             `json:"type"`
	Values      []GalleryAttrValue `json:"values,omitempty"`
	Label       string             `json:"label"`
	Description string             `json:"description,omitempty"`
	Usage       GalleryUsage       `json:"usage"`
}

// GalleryAttrSource 是某个已注册属性来源的聚合视图：来源 id 加上它为某知识库
// 提供的属性。
type GalleryAttrSource struct {
	ID    string           `json:"id"`
	Attrs []GalleryAttrDef `json:"attrs"`
}

// gallerySourceIDPattern 约束来源 id，使命名空间属性 id “<来源ID>:<名称>”
// 保持无歧义（分隔符为 “:”）。
var gallerySourceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var (
	galleryAttrSourcesMu sync.RWMutex
	galleryAttrSources   []galleryAttrSourceEntry
)

type galleryAttrSourceEntry struct {
	id string
	// fn 解析该来源为某知识库提供的属性。kbID 让未来的“按知识库”来源
	// （例如知识库自定义属性）可以参与；全局来源会忽略它。
	fn func(kbID string) []GalleryAttrDef
}

// RegisterGalleryAttrSource 向画廊的开放注册表添加一个属性来源。注册顺序即优先
// 顺序：当两个来源声明了相同的来源内属性名，命名空间 id 会让两者都保留
// （“system:keyword” 与 “kb:keyword”），并列时更早注册的来源在裸键查找中
// 获胜。请在 init() 中调用；fn 为 nil、id 畸形或重复都属于编程错误，会直接
// panic。
func RegisterGalleryAttrSource(id string, fn func(kbID string) []GalleryAttrDef) {
	if fn == nil || !gallerySourceIDPattern.MatchString(id) {
		panic("gallery: invalid attribute source registration (id=" + id + ")")
	}
	galleryAttrSourcesMu.Lock()
	defer galleryAttrSourcesMu.Unlock()
	for _, e := range galleryAttrSources {
		if e.id == id {
			panic("gallery: duplicate attribute source id: " + id)
		}
	}
	galleryAttrSources = append(galleryAttrSources, galleryAttrSourceEntry{id: id, fn: fn})
}

// GalleryAttrSources 按注册（优先）顺序解析为某知识库注册的每个来源。某个来源
// 返回空时产生空属性列表，而非错误——来源缺失是正常状态（例如系统属性管道尚未
// 部署）。
func GalleryAttrSources(kbID string) []GalleryAttrSource {
	galleryAttrSourcesMu.RLock()
	defer galleryAttrSourcesMu.RUnlock()
	out := make([]GalleryAttrSource, 0, len(galleryAttrSources))
	for _, e := range galleryAttrSources {
		defs := e.fn(kbID)
		if defs == nil {
			defs = []GalleryAttrDef{}
		}
		out = append(out, GalleryAttrSource{ID: e.id, Attrs: defs})
	}
	return out
}

// GalleryAttrSourceIDs 按优先顺序列出已注册的来源 id。
func GalleryAttrSourceIDs() []string {
	galleryAttrSourcesMu.RLock()
	defer galleryAttrSourcesMu.RUnlock()
	out := make([]string, 0, len(galleryAttrSources))
	for _, e := range galleryAttrSources {
		out = append(out, e.id)
	}
	return out
}

// GalleryAttrID 把来源 id 与来源内属性名拼接成命名空间 id，供画廊契约与每个
// 配置层使用。
func GalleryAttrID(sourceID, name string) string {
	return sourceID + ":" + name
}

// SplitGalleryAttrID 把命名空间属性 id 拆回来源 id 与来源内属性名。对没有分隔符
// 的 id，ok 为 false。
func SplitGalleryAttrID(id string) (sourceID, name string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return id[:i], id[i+1:], i > 0 && i+1 < len(id)
		}
	}
	return "", "", false
}
