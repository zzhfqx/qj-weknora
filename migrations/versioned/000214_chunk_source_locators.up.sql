-- 000114：为 chunk 增加原文定位信息。
-- source_locators 存储 chunk 在原始文件中的位置（PDF 页码与区域、Word 段落、
-- 表格行、文本字符区间等）的 JSON 列表，供引用导航打开原文并高亮。
-- 解析器没有报告位置时为空（NULL）。
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS source_locators JSONB;
