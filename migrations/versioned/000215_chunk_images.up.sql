-- 迁移 000215：chunk_images，图片画廊对 image_info 的投影表
-- （对应官方 0.8.3 的 000113；本地编号曾先定为 000213，因存量库已在 000214
--  之后、golang-migrate 只向前跑而被跳过，重排到 000215 以补跑）
--
-- 画廊用于列出某个知识库下的图片：每个 URL 一张，可按图片描述 / OCR 文本 /
-- 观察到的属性筛选、排序、分页。这些数据原本以 JSON 形式存放在 chunks.image_info
-- 里（每个 chunk 一个数组，同一张图片会在它的 image_ocr、image_caption 子 chunk
-- 上重复出现），因此直接从 chunks 取一页数据，需要解析该知识库下的每一个图片 chunk。
--
-- chunk_images 为每个 image_info 元素保存一行，并把字段拆成独立的列；chunks 上的
-- 触发器会让它跟随每一条写路径保持同步（插入、编辑、启停切换、软删与硬删、移动
-- 文档）。它只是一个普通投影：同一张图片的多个副本仍是各自独立的行，画廊查询通过
-- 带索引的反连接选取“最近更新的 chunk 上那个副本”。写入阶段不做任何决策，因此
-- 并发写入同一张图片无需协调。

CREATE TABLE IF NOT EXISTS chunk_images (
    chunk_id VARCHAR(36) NOT NULL,
    image_index INTEGER NOT NULL,
    tenant_id BIGINT NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    knowledge_id VARCHAR(36) NOT NULL,
    chunk_type VARCHAR(20) NOT NULL DEFAULT '',
    -- 去重键：优先 url，其次 original_url，最后用 “<chunk id>#<index>”。
    image_key TEXT NOT NULL,
    url TEXT NOT NULL DEFAULT '',
    original_url TEXT NOT NULL DEFAULT '',
    caption TEXT NOT NULL DEFAULT '',
    ocr_text TEXT NOT NULL DEFAULT '',
    -- 观察到的属性集合（image_info[i].attrs.attrs）。
    attrs JSONB NOT NULL DEFAULT '{}'::jsonb,
    is_enabled BOOLEAN NOT NULL DEFAULT true,
    status INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE,
    updated_at TIMESTAMP WITH TIME ZONE,
    PRIMARY KEY (chunk_id, image_index)
);

-- 按上传顺序列出，并列时以 chunk 与图片位置兜底。
CREATE INDEX IF NOT EXISTS idx_chunk_images_listing
    ON chunk_images(knowledge_base_id, tenant_id, created_at, chunk_id, image_index);
-- 同一张图片的多个副本，顺序即决定列出哪一个。
CREATE INDEX IF NOT EXISTS idx_chunk_images_copies
    ON chunk_images(knowledge_base_id, tenant_id, image_key, updated_at, chunk_id, image_index);

-- chunk_images_try_jsonb 解析 image_info；遇到畸形文本返回 NULL，
-- 这样一条坏数据既不会让 chunk 写入失败，也不会让本次回填失败。
CREATE OR REPLACE FUNCTION chunk_images_try_jsonb(raw TEXT) RETURNS JSONB
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    IF raw IS NULL OR raw NOT LIKE '[%' THEN
        RETURN NULL;
    END IF;
    RETURN raw::jsonb;
EXCEPTION WHEN others THEN
    RETURN NULL;
END $$;

-- chunk_images_sync 重新投影单个 chunk。若一次更新没有改动图片本身（状态、启停
-- 标志、时间戳），则就地修补对应行；这些列都不参与文本索引，因此不会引发索引抖动。
-- 其他任何改动则整体替换该 chunk 的投影行。
CREATE OR REPLACE FUNCTION chunk_images_sync() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    doc JSONB;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.deleted_at IS NULL AND NEW.deleted_at IS NULL
       AND OLD.image_info IS NOT DISTINCT FROM NEW.image_info
       AND OLD.tenant_id = NEW.tenant_id AND OLD.knowledge_base_id = NEW.knowledge_base_id
       AND OLD.knowledge_id = NEW.knowledge_id AND OLD.chunk_type IS NOT DISTINCT FROM NEW.chunk_type THEN
        UPDATE chunk_images
        SET is_enabled = NEW.is_enabled, status = COALESCE(NEW.status, 0),
            created_at = NEW.created_at, updated_at = NEW.updated_at
        WHERE chunk_id = NEW.id;
        RETURN NULL;
    END IF;

    IF TG_OP <> 'INSERT' THEN
        DELETE FROM chunk_images WHERE chunk_id = OLD.id;
    END IF;
    IF TG_OP <> 'DELETE' AND NEW.deleted_at IS NULL THEN
        doc := chunk_images_try_jsonb(NEW.image_info);
        IF jsonb_typeof(doc) = 'array' THEN
            INSERT INTO chunk_images (
                chunk_id, image_index, tenant_id, knowledge_base_id, knowledge_id, chunk_type,
                image_key, url, original_url, caption, ocr_text, attrs,
                is_enabled, status, created_at, updated_at
            )
            SELECT NEW.id, (e.idx - 1)::int, NEW.tenant_id, NEW.knowledge_base_id, NEW.knowledge_id,
                   COALESCE(NEW.chunk_type, ''),
                   COALESCE(NULLIF(e.img->>'url', ''), NULLIF(e.img->>'original_url', ''),
                            NEW.id || '#' || (e.idx - 1)),
                   COALESCE(e.img->>'url', ''), COALESCE(e.img->>'original_url', ''),
                   COALESCE(e.img->>'caption', ''), COALESCE(e.img->>'ocr_text', ''),
                   CASE WHEN jsonb_typeof(e.img->'attrs'->'attrs') = 'object'
                        THEN e.img->'attrs'->'attrs' ELSE '{}'::jsonb END,
                   NEW.is_enabled, COALESCE(NEW.status, 0), NEW.created_at, NEW.updated_at
            FROM jsonb_array_elements(doc) WITH ORDINALITY AS e(img, idx)
            WHERE jsonb_typeof(e.img) = 'object';
        END IF;
    END IF;
    RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS trg_chunk_images_insert ON chunks;
CREATE TRIGGER trg_chunk_images_insert AFTER INSERT ON chunks
    FOR EACH ROW WHEN (NEW.image_info LIKE '[%')
    EXECUTE FUNCTION chunk_images_sync();

-- 只有投影会复制的那些列发生变化才重新同步；image_info 既不是、也不会变成数组的
-- chunk 没有任何可投影内容。
DROP TRIGGER IF EXISTS trg_chunk_images_update ON chunks;
CREATE TRIGGER trg_chunk_images_update AFTER UPDATE ON chunks
    FOR EACH ROW WHEN (
        (OLD.image_info LIKE '[%' OR NEW.image_info LIKE '[%') AND (
            OLD.image_info IS DISTINCT FROM NEW.image_info
            OR OLD.is_enabled IS DISTINCT FROM NEW.is_enabled
            OR OLD.status IS DISTINCT FROM NEW.status
            OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
            OR OLD.tenant_id IS DISTINCT FROM NEW.tenant_id
            OR OLD.knowledge_base_id IS DISTINCT FROM NEW.knowledge_base_id
            OR OLD.knowledge_id IS DISTINCT FROM NEW.knowledge_id
            OR OLD.chunk_type IS DISTINCT FROM NEW.chunk_type
            OR OLD.created_at IS DISTINCT FROM NEW.created_at
            OR OLD.updated_at IS DISTINCT FROM NEW.updated_at
        )
    )
    EXECUTE FUNCTION chunk_images_sync();

DROP TRIGGER IF EXISTS trg_chunk_images_delete ON chunks;
CREATE TRIGGER trg_chunk_images_delete AFTER DELETE ON chunks
    FOR EACH ROW WHEN (OLD.image_info LIKE '[%')
    EXECUTE FUNCTION chunk_images_sync();

-- 回填所有未删除的图片 chunk。
INSERT INTO chunk_images (
    chunk_id, image_index, tenant_id, knowledge_base_id, knowledge_id, chunk_type,
    image_key, url, original_url, caption, ocr_text, attrs,
    is_enabled, status, created_at, updated_at
)
SELECT c.id, (e.idx - 1)::int, c.tenant_id, c.knowledge_base_id, c.knowledge_id, COALESCE(c.chunk_type, ''),
       COALESCE(NULLIF(e.img->>'url', ''), NULLIF(e.img->>'original_url', ''), c.id || '#' || (e.idx - 1)),
       COALESCE(e.img->>'url', ''), COALESCE(e.img->>'original_url', ''),
       COALESCE(e.img->>'caption', ''), COALESCE(e.img->>'ocr_text', ''),
       CASE WHEN jsonb_typeof(e.img->'attrs'->'attrs') = 'object'
            THEN e.img->'attrs'->'attrs' ELSE '{}'::jsonb END,
       c.is_enabled, COALESCE(c.status, 0), c.created_at, c.updated_at
FROM chunks c
CROSS JOIN LATERAL jsonb_array_elements(
    CASE WHEN jsonb_typeof(chunk_images_try_jsonb(c.image_info)) = 'array'
         THEN chunk_images_try_jsonb(c.image_info) ELSE '[]'::jsonb END
) WITH ORDINALITY AS e(img, idx)
WHERE c.deleted_at IS NULL AND c.image_info LIKE '[%' AND jsonb_typeof(e.img) = 'object'
ON CONFLICT (chunk_id, image_index) DO NOTHING;

-- caption / OCR 文本的关键字搜索在 pg_trgm 可用时使用 trigram 索引。该扩展是可选的：
-- 若无法使用（没有创建扩展的权限），搜索仍可工作，只是退化为顺序扫描。关闭
-- fastupdate，使批量导入的数据在下一次 vacuum 合并待处理列表之前，就能以全速被
-- 检索到。
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm;
EXCEPTION WHEN others THEN
    RAISE NOTICE '[迁移 000215] pg_trgm 不可用（%）；画廊关键字搜索将退化为顺序扫描', SQLERRM;
END $$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
        CREATE INDEX IF NOT EXISTS idx_chunk_images_caption_trgm
            ON chunk_images USING gin (caption gin_trgm_ops) WITH (fastupdate = off);
        CREATE INDEX IF NOT EXISTS idx_chunk_images_ocr_trgm
            ON chunk_images USING gin (ocr_text gin_trgm_ops) WITH (fastupdate = off);
    END IF;
END $$;
