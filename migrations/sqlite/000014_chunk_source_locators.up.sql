-- 原文定位信息，以 JSON 文本存储。
ALTER TABLE chunks ADD COLUMN source_locators TEXT;
