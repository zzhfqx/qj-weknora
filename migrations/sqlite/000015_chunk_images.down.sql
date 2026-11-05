-- 回滚 000015：删除 chunk_images 投影表及其触发器（对应官方 SQLite 000032）
DROP TRIGGER IF EXISTS trg_chunk_images_insert;
DROP TRIGGER IF EXISTS trg_chunk_images_patch;
DROP TRIGGER IF EXISTS trg_chunk_images_update;
DROP TRIGGER IF EXISTS trg_chunk_images_delete;
DROP TABLE IF EXISTS chunk_images;
