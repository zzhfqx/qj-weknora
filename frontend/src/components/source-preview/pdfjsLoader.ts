// 以 .js 文件名打包成 worker chunk，确保任何静态服务器都以 JavaScript MIME
// 类型返回它（module worker 会拒绝其他 MIME 类型）。
import pdfWorkerUrl from 'pdfjs-dist/legacy/build/pdf.worker.min.mjs?worker&url'

export type PdfJs = typeof import('pdfjs-dist/legacy/build/pdf.mjs')

let loading: Promise<PdfJs> | null = null

/**
 * 库只加载一次，但每个文档拥有自己的 worker。
 * 在上一个加载任务正在销毁时复用端口，会导致快速切换文档失败。
 */
export function loadPdfJs(): Promise<PdfJs> {
  if (!loading) {
    loading = import('pdfjs-dist/legacy/build/pdf.mjs').then((lib) => {
      lib.GlobalWorkerOptions.workerSrc = pdfWorkerUrl
      return lib
    })
    loading.catch(() => {
      loading = null
    })
  }
  return loading
}
