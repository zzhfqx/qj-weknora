/** 字节级身份：绝不根据文件名或 OCR 内容推断内嵌图片的位置。 */
export async function sourceImageDigest(bytes: ArrayBuffer): Promise<string> {
  const hash = await crypto.subtle.digest('SHA-256', bytes)
  return [...new Uint8Array(hash)].map((b) => b.toString(16).padStart(2, '0')).join('')
}

export async function indexEmbeddedSourceImages(root: Element): Promise<void> {
  await Promise.all(
    [...root.querySelectorAll<HTMLImageElement>('img')].map(async (image) => {
      // 只处理 docx-preview 内嵌的字节；绝不请求远程文档链接。
      const match = /^data:(?:image\/[^;,]+|application\/octet-stream);base64,([A-Za-z0-9+/=\s]+)$/.exec(image.src)
      if (!match) return
      try {
        const bytes = Uint8Array.from(atob(match[1]!), (c) => c.charCodeAt(0))
        image.dataset.sourceImageDigest = await sourceImageDigest(bytes.buffer)
      } catch {
        // 无法解码的图片不能作为身份依据。
      }
    }),
  )
}
