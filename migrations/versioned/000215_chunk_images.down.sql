-- 回滚 000215：删除 chunk_images 投影表及其触发器/函数（对应官方 000113）
DROP TRIGGER IF EXISTS trg_chunk_images_insert ON chunks;
DROP TRIGGER IF EXISTS trg_chunk_images_update ON chunks;
DROP TRIGGER IF EXISTS trg_chunk_images_delete ON chunks;
DROP FUNCTION IF EXISTS chunk_images_sync();
DROP FUNCTION IF EXISTS chunk_images_try_jsonb(TEXT);
DROP TABLE IF EXISTS chunk_images;
