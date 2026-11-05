import {
  buildProtectedFileRequest,
  isProviderFileURL,
  resolveProtectedFileAccess,
  type ProtectedFileRequest,
} from '../../../utils/protectedFileAccess'

/**
 * 加载一张画廊图片的代理请求；当浏览器可直接渲染该 URL 时返回 null。
 *
 * 存储中的图片是存储句柄（local://、minio://、cos://、s3://、
 * storage://<backend>/...、resource://），没有浏览器能直接打开；它们要经
 * 知识库文件代理，该代理同时也是对从其他租户共享来的 KB 查看者的鉴权点。
 * 方案列表由 protectedFileAccess 持有，因此画廊不会与聊天、wiki 漂移。
 */
export function galleryImageRequest(rawUrl: string, kbId: string): ProtectedFileRequest | null {
  if (!rawUrl || !isProviderFileURL(rawUrl)) return null
  return buildProtectedFileRequest(rawUrl, resolveProtectedFileAccess({ mode: 'knowledgeBase', kbId }))
}
