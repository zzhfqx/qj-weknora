import assert from 'node:assert/strict'
import test from 'node:test'

import { galleryImageRequest } from './galleryImageSrc.ts'

const KB_PROXY = '/api/v1/knowledge-bases/kb-1/files?file_path='

// 默认本地存储写入 local:// 句柄；它与其他所有存储方案都必须到达知识库代理，
// 而不是 <img src>。
for (const url of [
  'local://10001/exports/a.png',
  'minio://bucket/a.png',
  'cos://bucket/a.png',
  's3://bucket/a.png',
  'resource://AbCdEfGhIjKlMnOpQrStUv',
  'storage://backend-1/local://10001/a.png',
]) {
  test(`${url.split('://')[0]}:// images load through the knowledge-base file proxy`, () => {
    assert.equal(galleryImageRequest(url, 'kb-1')?.url, `${KB_PROXY}${encodeURIComponent(url)}`)
  })
}

test('browser-renderable URLs are used as is', () => {
  assert.equal(galleryImageRequest('https://cdn.example.com/a.png', 'kb-1'), null)
  assert.equal(galleryImageRequest('', 'kb-1'), null)
})
