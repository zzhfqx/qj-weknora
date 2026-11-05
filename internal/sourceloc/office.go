package sourceloc

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/xuri/excelize/v2"
	"golang.org/x/net/html"

	"github.com/Tencent/WeKnora/internal/types"
)

// maxUnits 限制从单个文件提取的结构片段数量。
const maxUnits = 200000

// maxZipEntryBytes 限制提取时读取的单个压缩包成员大小。
const maxZipEntryBytes = 64 << 20

// SupportsStructure 报告 StructureUnits 能否读取该文件类型。
func SupportsStructure(fileType string) bool {
	switch normalizeType(fileType) {
	case "docx", "docm", "pptx", "pptm", "xlsx", "xlsm", "csv", "epub":
		return true
	}
	return false
}

// AlignStructure 读取 Office 文件的结构，并把解析器的 Markdown 与之对齐。
// 文件类型不受支持或没有任何内容能定位时返回 nil。
func AlignStructure(fileType string, data []byte, markdown string) ([]types.SourceBlock, error) {
	units, err := StructureUnits(fileType, data)
	if err != nil || len(units) == 0 {
		return nil, err
	}
	return Align(markdown, units), nil
}

// StructureUnits 按文档顺序列出 Office 文件的结构片段，每个片段带有
// 它对应的定位器。
func StructureUnits(fileType string, data []byte) ([]Unit, error) {
	switch normalizeType(fileType) {
	case "docx", "docm":
		return docxUnits(data)
	case "pptx", "pptm":
		return pptxUnits(data)
	case "xlsx", "xlsm":
		return xlsxUnits(data)
	case "csv":
		return csvUnits(data)
	case "epub":
		return epubUnits(data)
	}
	return nil, nil
}

func normalizeType(fileType string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(fileType), "."))
}

// ---- docx -----------------------------------------------------------------

// docxUnits 遍历 word/document.xml。每个正文级段落和表格算作一个块，
// 顺序与 Word 的排版顺序一致；内容控件（w:sdt）是透明的，与 docx-preview
// 渲染器保持一致。表格按行产生一个片段，因此长表格仍然可以逐行对齐。
func docxUnits(data []byte) ([]Unit, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	doc, err := readZipFile(zr, "word/document.xml")
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var (
		units    []Unit
		depth    int // w:body 之下的元素深度；sdt 包装不计入
		inBody   bool
		block    int
		tblDepth int
		text     strings.Builder
		inText   bool
		skipText int
	)
	flush := func() {
		t := strings.TrimSpace(text.String())
		text.Reset()
		if t != "" && len(units) < maxUnits {
			loc := types.SourceLocator{Type: types.SourceLocatorDocx, Block: block}
			units = append(units, Unit{Text: t, Locator: loc})
		}
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return units, nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if !inBody {
				if name == "body" {
					inBody = true
				}
				continue
			}
			if name == "sdt" || name == "sdtContent" || name == "sdtPr" || name == "sdtEndPr" {
				if name == "sdtPr" || name == "sdtEndPr" {
					skipText++
				}
				continue
			}
			depth++
			if depth == 1 && (name == "p" || name == "tbl") {
				block++
				text.Reset()
			}
			switch name {
			case "tbl":
				tblDepth++
			case "t":
				inText = true
			case "tab":
				text.WriteByte(' ')
			case "br", "cr":
				text.WriteByte(' ')
			case "delText", "instrText":
				skipText++
			}
		case xml.EndElement:
			name := t.Name.Local
			if !inBody {
				continue
			}
			if name == "body" {
				inBody = false
				continue
			}
			if name == "sdt" || name == "sdtContent" || name == "sdtPr" || name == "sdtEndPr" {
				if name == "sdtPr" || name == "sdtEndPr" {
					skipText--
				}
				continue
			}
			switch name {
			case "t":
				inText = false
			case "delText", "instrText":
				skipText--
			case "tr":
				if tblDepth == 1 {
					flush()
				}
			case "tc":
				text.WriteByte(' ')
			case "tbl":
				tblDepth--
			}
			if depth == 1 && name == "p" && tblDepth == 0 {
				flush()
			}
			depth--
		case xml.CharData:
			if inText && skipText == 0 {
				text.Write(t)
			}
		}
	}
	return units, nil
}

// ---- pptx -----------------------------------------------------------------

type pptxPresentation struct {
	SlideIDs []struct {
		RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
	} `xml:"sldIdLst>sldId"`
}

type opcRelationships struct {
	Items []struct {
		ID     string `xml:"Id,attr"`
		Type   string `xml:"Type,attr"`
		Target string `xml:"Target,attr"`
	} `xml:"Relationship"`
}

// pptxUnits 按演示顺序列出每张幻灯片及其演讲者备注中的每个文本段落。
// 以段落而不是整张幻灯片为片段，这样即使解析器调整了形状顺序也仍能对齐。
func pptxUnits(data []byte) ([]Unit, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	presXML, err := readZipFile(zr, "ppt/presentation.xml")
	if err != nil {
		return nil, err
	}
	var pres pptxPresentation
	if err := xml.Unmarshal(presXML, &pres); err != nil {
		return nil, err
	}
	rels, err := readRels(zr, "ppt/_rels/presentation.xml.rels")
	if err != nil {
		return nil, err
	}
	var units []Unit
	for i, sld := range pres.SlideIDs {
		target, ok := rels[sld.RID]
		if !ok {
			continue
		}
		slidePath := resolvePart("ppt", target.Target)
		slideXML, err := readZipFile(zr, slidePath)
		if err != nil {
			continue
		}
		loc := types.SourceLocator{Type: types.SourceLocatorSlide, Slide: i + 1}
		for _, p := range drawingParagraphs(slideXML) {
			units = append(units, Unit{Text: p, Locator: loc})
		}
		// 所有用到的解析器都把演讲者备注排在幻灯片之后。
		slideRels, err := readRels(zr, relsPathFor(slidePath))
		if err == nil {
			for _, rel := range slideRels {
				if strings.HasSuffix(rel.Type, "/notesSlide") {
					notesXML, err := readZipFile(zr, resolvePart(path.Dir(slidePath), rel.Target))
					if err == nil {
						for _, p := range drawingParagraphs(notesXML) {
							units = append(units, Unit{Text: p, Locator: loc})
						}
					}
				}
			}
		}
		if len(units) >= maxUnits {
			break
		}
	}
	return units, nil
}

// drawingParagraphs 返回每个 DrawingML 段落（a:p）的文本。
func drawingParagraphs(doc []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var (
		out    []string
		text   strings.Builder
		inText bool
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "br":
				text.WriteByte(' ')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if s := strings.TrimSpace(text.String()); s != "" {
					out = append(out, s)
				}
				text.Reset()
			}
		case xml.CharData:
			if inText {
				text.Write(t)
			}
		}
	}
	return out
}

// ---- xlsx / csv -----------------------------------------------------------

// xlsxUnits 列出每张工作表中每个非空行的行号。行采用流式读取，
// 大工作簿不会被整体载入内存；列满 maxUnits 行后停止。
func xlsxUnits(data []byte) ([]Unit, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var units []Unit
	for _, sheet := range f.GetSheetList() {
		units = appendSheetUnits(f, sheet, units)
		if len(units) >= maxUnits {
			break
		}
	}
	return units, nil
}

// appendSheetUnits 追加一张工作表的非空行，总数不超过 maxUnits。
func appendSheetUnits(f *excelize.File, sheet string, units []Unit) []Unit {
	rows, err := f.Rows(sheet)
	if err != nil {
		return units
	}
	defer func() { _ = rows.Close() }()
	// 迭代器会访问每个行号，包括缺失的行。
	for row := 1; rows.Next() && len(units) < maxUnits; row++ {
		cells, err := rows.Columns()
		if err != nil {
			break
		}
		text := strings.TrimSpace(strings.Join(cells, " "))
		if text == "" {
			continue
		}
		units = append(units, Unit{Text: text, Locator: types.SourceLocator{
			Type: types.SourceLocatorSheet, Sheet: sheet, RowStart: row, RowEnd: row,
		}})
	}
	return units
}

// csvUnits 把 CSV 文件的每条记录列为其唯一工作表中的一行。
func csvUnits(data []byte) ([]Unit, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1
	var units []Unit
	for row := 1; ; row++ {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return units, nil
		}
		text := strings.TrimSpace(strings.Join(rec, " "))
		if text == "" {
			continue
		}
		units = append(units, Unit{Text: text, Locator: types.SourceLocator{
			Type: types.SourceLocatorSheet, RowStart: row, RowEnd: row,
		}})
		if len(units) >= maxUnits {
			break
		}
	}
	return units, nil
}

// ---- epub -----------------------------------------------------------------

type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type epubPackage struct {
	Manifest []struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	} `xml:"manifest>item"`
	Spine []struct {
		IDRef string `xml:"idref,attr"`
	} `xml:"spine>itemref"`
}

// epubUnits 列出每个 spine 项的块级文本。章节号是该项在 spine 中从 1 开始
// 的位置；标题是它的第一个标题元素。
func epubUnits(data []byte) ([]Unit, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	containerXML, err := readZipFile(zr, "META-INF/container.xml")
	if err != nil {
		return nil, err
	}
	var container epubContainer
	if err := xml.Unmarshal(containerXML, &container); err != nil || len(container.Rootfiles) == 0 {
		return nil, errors.New("epub: 找不到 rootfile")
	}
	opfPath := container.Rootfiles[0].FullPath
	opfXML, err := readZipFile(zr, opfPath)
	if err != nil {
		return nil, err
	}
	var pkg epubPackage
	if err := xml.Unmarshal(opfXML, &pkg); err != nil {
		return nil, err
	}
	hrefs := make(map[string]string, len(pkg.Manifest))
	for _, item := range pkg.Manifest {
		hrefs[item.ID] = item.Href
	}
	var units []Unit
	for i, ref := range pkg.Spine {
		href, ok := hrefs[ref.IDRef]
		if !ok {
			continue
		}
		doc, err := readZipFile(zr, resolvePart(path.Dir(opfPath), stripFragment(href)))
		if err != nil {
			continue
		}
		title, blocks := htmlBlocks(doc)
		loc := types.SourceLocator{Type: types.SourceLocatorSection, Section: i + 1, Title: title}
		for _, b := range blocks {
			units = append(units, Unit{Text: b, Locator: loc})
		}
		if len(units) >= maxUnits {
			break
		}
	}
	return units, nil
}

// htmlBlocks 返回 XHTML 文档的第一个标题和每个块级元素的文本。
func htmlBlocks(doc []byte) (string, []string) {
	z := html.NewTokenizer(bytes.NewReader(doc))
	var (
		title     string
		blocks    []string
		text      strings.Builder
		inHeading bool
		skip      int
	)
	flush := func() {
		if s := strings.Join(strings.Fields(text.String()), " "); s != "" {
			blocks = append(blocks, s)
			if inHeading && title == "" {
				title = s
			}
		}
		text.Reset()
	}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			flush()
			return title, blocks
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch tag {
			case "script", "style", "head":
				if tt == html.StartTagToken {
					skip++
				} else if tt == html.EndTagToken && skip > 0 {
					skip--
				}
				continue
			}
			if isHTMLBlock(tag) {
				flush()
				if strings.HasPrefix(tag, "h") && len(tag) == 2 {
					inHeading = tt == html.StartTagToken
				}
			}
		case html.TextToken:
			if skip == 0 {
				text.Write(z.Text())
				text.WriteByte(' ')
			}
		}
	}
}

func isHTMLBlock(tag string) bool {
	switch tag {
	case "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6",
		"blockquote", "pre", "section", "article", "table", "dt", "dd", "br", "figcaption":
		return true
	}
	return false
}

// ---- zip 辅助函数 ----------------------------------------------------------

func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			if f.UncompressedSize64 > maxZipEntryBytes {
				return nil, fmt.Errorf("%s: 成员过大", name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return io.ReadAll(io.LimitReader(rc, maxZipEntryBytes))
		}
	}
	return nil, fmt.Errorf("%s: 不存在", name)
}

func readRels(zr *zip.Reader, name string) (map[string]struct{ Type, Target string }, error) {
	raw, err := readZipFile(zr, name)
	if err != nil {
		return nil, err
	}
	var rels opcRelationships
	if err := xml.Unmarshal(raw, &rels); err != nil {
		return nil, err
	}
	out := make(map[string]struct{ Type, Target string }, len(rels.Items))
	for _, r := range rels.Items {
		out[r.ID] = struct{ Type, Target string }{r.Type, r.Target}
	}
	return out, nil
}

// relsPathFor 返回一个 OPC 部件的关系部件路径，例如
// ppt/slides/slide1.xml -> ppt/slides/_rels/slide1.xml.rels。
func relsPathFor(part string) string {
	return path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
}

// resolvePart 以源部件所在目录为基准解析关系目标。绝对目标以包根为根。
func resolvePart(dir, target string) string {
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(path.Clean(target), "/")
	}
	return path.Clean(path.Join(dir, target))
}

func stripFragment(href string) string {
	if i := strings.IndexByte(href, '#'); i >= 0 {
		return href[:i]
	}
	return href
}
