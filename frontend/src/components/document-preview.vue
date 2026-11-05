// @ts-nocheck
<script setup lang="ts">
import { indexEmbeddedSourceImages } from '@/utils/sourceImage';
import { ref, shallowRef, watch, onUnmounted, nextTick, defineAsyncComponent } from 'vue';
import { previewKnowledgeFile } from '@/api/knowledge-base/index';
import { previewTemporaryAttachment } from '@/api/chat/temporary-attachments';
import { downloadArtifact } from '@/api/chat';
import hljs from 'highlight.js';
import 'highlight.js/styles/github.css';
import 'katex/dist/katex.min.css';
import { useI18n } from 'vue-i18n';
import { sanitizeHTML, sanitizeMarkdownHTML } from '@/utils/security';
import { renderDocumentPreviewMarkdown } from '@/utils/documentPreviewMarkdown';
import { openMermaidFullscreen } from '@/utils/mermaidViewer';
import { renderMermaidToSvg } from '@/utils/mermaidShared';
import { findMarkdownSourceRange } from '@/utils/markdownSourceLocate';
import {
  findInText,
  sourceQuoteText,
  type SourceLocateRequest,
} from '@/utils/sourceLocator';
import {
  buildTextIndex,
  findTextRange,
  highlightElements,
  highlightRanges,
  rangeForOffsets,
  scrollRectIntoContainer,
} from '@/utils/sourceLocatorDom';
import {
  FILE_PREVIEW_SNIFF_BYTES,
  getHighlightLang as resolveHighlightLang,
  getPreviewMimeType,
  prettyPrintJson,
  resolveFilePreviewExt,
  resolvePreviewKind,
  shouldPrettyPrintJson,
  sniffPreview,
  isValidUTF8,
  type FilePreviewKind,
} from '@/utils/filePreview';


const VueOfficePptx = defineAsyncComponent(() => import('@vue-office/pptx'));
const PdfSourceViewer = defineAsyncComponent(() => import('@/components/source-preview/PdfSourceViewer.vue'));

const { t } = useI18n();

const props = defineProps<{
  sourceBlob?: Blob;
  knowledgeId?: string;
  sessionId?: string;
  attachmentId?: string;
  messageId?: string;
  artifactIndex?: number;
  fileType: string;
  fileName: string;
  active: boolean;
  fillHeight?: boolean;
  /** 将预览操作按钮放到宿主头部下载按钮旁。 */
  toolbarTarget?: HTMLElement | null;
  /**
   * 原文模式使用 pdf.js（而不是浏览器内置查看器）渲染 PDF，
   * 以便滚动到引用位置并高亮。
   */
  sourceMode?: boolean;
  /** 文件渲染完成后需要滚动到的位置与高亮内容。 */
  locate?: SourceLocateRequest | null;
}>();

export type SourceLocateResult = { found: boolean; precise: boolean; granularity?: string; reason?: string };

const emit = defineEmits<{
  located: [result: SourceLocateResult];
}>();

const loading = ref(false);
const error = ref('');
const previewType = ref<FilePreviewKind>('unsupported');
const blobUrl = ref('');
const textContent = ref('');
const highlightedCode = ref('');
const markdownHtml = ref('');
const excelHtml = ref('');
/** 工作表名称列表，用于把定位器中的工作表名映射到渲染出的表格。 */
const excelSheetNames = ref<string[]>([]);
/** 当前预览实际使用的扩展名（嗅探后可能与文件名不同）。 */
const previewExt = ref('');
const mermaidSvg = ref('');
const htmlViewMode = ref<'render' | 'source'>('render');
const pptxData = shallowRef<ArrayBuffer | null>(null);
const pdfData = shallowRef<ArrayBuffer | null>(null);
const docxContainer = ref<HTMLElement | null>(null);
const imageNaturalWidth = ref(0);
const imageNaturalHeight = ref(0);
let loadedForId = '';

const isFullscreen = ref(false);
const previewContent = ref<HTMLElement | null>(null);

function toggleFullscreen() {
  isFullscreen.value = !isFullscreen.value;
  if (isFullscreen.value) {
    document.body.style.overflow = 'hidden';
  } else {
    document.body.style.overflow = '';
  }
}


function ensureBlobType(blob: Blob, ft: string): Blob {
  const expected = getPreviewMimeType(ft);
  if (blob.type === expected) return blob;
  return new Blob([blob], { type: expected });
}

function getHighlightLang(ft: string): string {
  return resolveHighlightLang(ft);
}

async function renderDocx(blob: Blob) {
  const { renderAsync } = await import('docx-preview');
  if (docxContainer.value) {
    docxContainer.value.innerHTML = '';
    await renderAsync(blob, docxContainer.value, undefined, {
      className: 'docx-preview-wrapper',
      inWrapper: true,
      ignoreWidth: false,
      ignoreHeight: false,
      ignoreFonts: false,
      breakPages: true,
      ignoreLastRenderedPageBreak: true,
      experimental: false,
      trimXmlDeclaration: true,
      useBase64URL: true,
    });
    // 为内嵌图片建立字节摘要，供引用按图片身份定位。
    await indexEmbeddedSourceImages(docxContainer.value);
  }
}

function decodeCSVBlob(arrayBuffer: ArrayBuffer): string {
  const bytes = new Uint8Array(arrayBuffer);
  if (bytes[0] === 0xEF && bytes[1] === 0xBB && bytes[2] === 0xBF) {
    return new TextDecoder('utf-8').decode(bytes);
  }
  if (isValidUTF8(bytes)) {
    return new TextDecoder('utf-8').decode(bytes);
  }
  return new TextDecoder('gbk').decode(bytes);
}

async function renderExcel(blob: Blob, fileType?: string) {
  const XLSX = await import('xlsx');
  const arrayBuffer = await blob.arrayBuffer();

  let workbook;
  const lowerType = fileType?.toLowerCase();
  if (lowerType === 'csv') {
    const csvText = decodeCSVBlob(arrayBuffer);
    workbook = XLSX.read(csvText, { type: 'string' });
  } else if (lowerType === 'tsv' || lowerType === 'tab') {
    const tsvText = decodeCSVBlob(arrayBuffer);
    workbook = XLSX.read(tsvText, { type: 'string', FS: '\t' });
  } else {
    workbook = XLSX.read(arrayBuffer, { type: 'array' });
  }

  excelSheetNames.value = [...workbook.SheetNames];
  let html = '';
  workbook.SheetNames.forEach((name, sheetIdx) => {
    const sheet = workbook.Sheets[name];
    const sheetHtml = XLSX.utils.sheet_to_html(sheet, { id: `sheet-${sheetIdx}` });
    html += `<div class="excel-sheet">`;
    if (workbook.SheetNames.length > 1) {
      html += `<div class="excel-sheet-name">${name}</div>`;
    }
    html += sheetHtml;
    html += `</div>`;
  });
  excelHtml.value = sanitizeHTML(html);
}

async function renderText(blob: Blob, fileType: string) {
  let text = await blob.text();
  if (shouldPrettyPrintJson(fileType)) {
    text = prettyPrintJson(text);
  }
  textContent.value = text;

  const lang = getHighlightLang(fileType);
  if (lang && hljs.getLanguage(lang)) {
    try {
      highlightedCode.value = hljs.highlight(text, { language: lang }).value;
      return;
    } catch { /* fallthrough */ }
  }
  const auto = hljs.highlightAuto(text);
  highlightedCode.value = auto.value;
}

async function renderMarkdown(blob: Blob) {
  const text = await blob.text();

  // 校验文本内容是否有效
  if (!text || typeof text !== 'string') {
    markdownHtml.value = '<p style="color: var(--td-text-color-disabled); text-align: center; padding: 20px;">文档内容为空</p>';
    return;
  }

  markdownHtml.value = renderDocumentPreviewMarkdown(text);
}

function onImageLoad(e: Event) {
  const img = e.target as HTMLImageElement;
  imageNaturalWidth.value = img.naturalWidth;
  imageNaturalHeight.value = img.naturalHeight;
}

function getPreviewSourceKey(): string {
  if (props.sourceBlob) {
    return `resource-blob:${props.fileName}:${props.fileType}:${props.sourceBlob.size}:${props.sourceBlob.type}`;
  }
  if (props.knowledgeId) return `knowledge:${props.knowledgeId}`;
  if (props.sessionId && props.attachmentId) return `attachment:${props.sessionId}:${props.attachmentId}`;
  if (
    props.sessionId &&
    props.messageId &&
    Number.isInteger(props.artifactIndex) &&
    (props.artifactIndex as number) >= 0
  ) {
    return `artifact:${props.sessionId}:${props.messageId}:${props.artifactIndex}`;
  }
  return '';
}

// Skill-generated HTML is often a self-contained chart that needs its own
// scripts. Knowledge-base files and chat attachments are untrusted uploads:
// they stay as source, matching the previous text preview.
function allowsHtmlScriptPreview(): boolean {
  return getPreviewSourceKey().startsWith('artifact:');
}

async function fetchPreviewBlob(): Promise<Blob> {
  if (props.sourceBlob) return props.sourceBlob;
  if (props.knowledgeId) {
    return previewKnowledgeFile(props.knowledgeId);
  }
  if (props.sessionId && props.attachmentId) {
    return previewTemporaryAttachment(props.sessionId, props.attachmentId);
  }
  if (
    props.sessionId &&
    props.messageId &&
    Number.isInteger(props.artifactIndex) &&
    (props.artifactIndex as number) >= 0
  ) {
    return downloadArtifact(props.sessionId, props.messageId, props.artifactIndex as number);
  }
  throw new Error('Missing preview source');
}

async function renderMermaid(blob: Blob) {
  const text = await blob.text();
  const svg = await renderMermaidToSvg(text, `file-preview-mermaid-${Date.now()}`);
  if (svg) {
    mermaidSvg.value = sanitizeMarkdownHTML(svg);
    return;
  }
  previewType.value = 'text';
  await renderText(new Blob([text], { type: 'text/plain' }), 'mmd');
}

function openMermaid() {
  if (mermaidSvg.value) openMermaidFullscreen(mermaidSvg.value);
}

async function loadPreview() {
  const sourceKey = getPreviewSourceKey();
  if (!sourceKey) return;
  if (loadedForId === sourceKey) return;

  cleanup();
  loading.value = true;
  error.value = '';
  htmlViewMode.value = allowsHtmlScriptPreview() ? 'render' : 'source';

  let ft = resolveFilePreviewExt(props.fileName, props.fileType);
  previewType.value = resolvePreviewKind(ft);

  try {
    const rawBlob = await fetchPreviewBlob();
    let kind = resolvePreviewKind(ft);
    if (kind === 'unsupported') {
      const sample = new Uint8Array(await rawBlob.slice(0, FILE_PREVIEW_SNIFF_BYTES).arrayBuffer());
      const sniffed = sniffPreview(sample);
      kind = sniffed.kind;
      if (sniffed.ext) ft = sniffed.ext;
    }
    previewType.value = kind;
    previewExt.value = ft;

    if (kind === 'unsupported') {
      loading.value = false;
      return;
    }

    const blob = ensureBlobType(rawBlob, ft);
    loadedForId = sourceKey;

    loading.value = false;
    await nextTick();

    switch (kind) {
      case 'pdf': {
        if (props.sourceMode) {
          // 原文模式交给 pdf.js，以便定位与高亮。
          pdfData.value = await blob.arrayBuffer();
        } else {
          blobUrl.value = URL.createObjectURL(blob);
        }
        break;
      }
      case 'image':
      case 'audio':
      case 'video': {
        blobUrl.value = URL.createObjectURL(blob);
        break;
      }
      case 'html': {
        if (allowsHtmlScriptPreview()) {
          blobUrl.value = URL.createObjectURL(blob);
        }
        await renderText(blob, ft || 'html');
        break;
      }
      case 'docx': {
        await renderDocx(blob);
        break;
      }
      case 'excel': {
        await renderExcel(blob, ft);
        break;
      }
      case 'text': {
        await renderText(blob, ft);
        break;
      }
      case 'markdown': {
        await renderMarkdown(blob);
        break;
      }
      case 'pptx': {
        pptxData.value = await blob.arrayBuffer();
        break;
      }
      case 'mermaid': {
        await renderMermaid(blob);
        break;
      }
    }
    // PPTX 异步渲染，由渲染完成事件触发定位。
    if (kind !== 'pptx') scheduleLocate();
  } catch (err: any) {
    console.error('Document preview failed:', err);
    error.value = err?.message || t('preview.loadFailed');
  } finally {
    loading.value = false;
  }
}

function cleanup() {
  if (blobUrl.value) {
    URL.revokeObjectURL(blobUrl.value);
    blobUrl.value = '';
  }
  textContent.value = '';
  highlightedCode.value = '';
  markdownHtml.value = '';
  excelHtml.value = '';
  excelSheetNames.value = [];
  previewExt.value = '';
  mermaidSvg.value = '';
  htmlViewMode.value = 'render';
  pptxData.value = null;
  pdfData.value = null;
  imageNaturalWidth.value = 0;
  imageNaturalHeight.value = 0;
  loadedForId = '';
  clearLocateMarks();
  if (docxContainer.value) {
    docxContainer.value.innerHTML = '';
  }
}

// ── 引用原文定位 ──
// 每种预览类型以自己的方式揭示 SourceLocateRequest：先使用结构化定位器
// （页码、块、幻灯片、行、章节、时间），再用引文文本在缩小的范围内匹配，
// 最后才在整个预览中查找引文。

type ImageMark = { left: number; top: number; width: number; height: number };
const imageMarks = ref<ImageMark[]>([]);
const audioQuote = ref('');
const audioRange = ref('');
let clearLocate: Array<() => void> = [];

function clearLocateMarks() {
  for (const clear of clearLocate) clear();
  clearLocate = [];
  imageMarks.value = [];
  audioQuote.value = '';
  audioRange.value = '';
}

let locateScheduled = false;
function scheduleLocate() {
  if (!props.locate || locateScheduled) return;
  locateScheduled = true;
  void nextTick(() => {
    locateScheduled = false;
    applyLocate();
  });
}

watch(
  () => props.locate?.token,
  () => {
    if (loadedForId && !loading.value) scheduleLocate();
  },
);

const NOT_FOUND: SourceLocateResult = { found: false, precise: false };

function findQuoteRange(root: Node | null | undefined, quotes: string[]): Range | null {
  if (!root) return null;
  const index = buildTextIndex(root);
  for (const quote of quotes) {
    const range = findTextRange(root, quote, index);
    if (range) return range;
  }
  return null;
}

function reveal(container: Element | null | undefined, target: Range | Element | null | undefined) {
  if (!container || !target) return;
  scrollRectIntoContainer(container, target.getBoundingClientRect());
}

function commitMarks(ranges: Range[], elements: Element[]) {
  if (ranges.length) clearLocate.push(highlightRanges(ranges));
  if (elements.length) clearLocate.push(highlightElements(elements));
}

/** 在 `root` 下揭示找到的第一段引文，并滚动 `container`。 */
function locateQuote(root: Element | null | undefined, container: Element | null | undefined, quotes: string[]): SourceLocateResult {
  const range = findQuoteRange(root, quotes);
  if (!range) return NOT_FOUND;
  commitMarks([range], []);
  reveal(container, range);
  return { found: true, precise: true };
}

/** 只有精确证据才能缩小结构化原文区域。 */
function evidenceQuotes(request: SourceLocateRequest): string[] {
  return request.scope ? [request.scope] : request.quotes;
}

function locateDocx(request: SourceLocateRequest): SourceLocateResult {
  const root = docxContainer.value;
  if (!root) return NOT_FOUND;
  const blocks = Array.from(root.querySelectorAll('section > article')).flatMap((article) =>
    Array.from(article.children).filter((c) => c.tagName === 'P' || c.tagName === 'TABLE'),
  );
  if (request.imageDigest) {
    const images = [...root.querySelectorAll('img')].filter((image) => image.dataset.sourceImageDigest === request.imageDigest);
    if (images.length !== 1) return { ...NOT_FOUND, ...(images.length > 1 ? { reason: 'ambiguous' } : {}) };
    commitMarks([], images);
    reveal(root, images[0]);
    return { found: true, precise: false, granularity: 'block' };
  }
  if (request.imageContext) {
    const { before: beforeQuote, after: afterQuote } = request.imageContext;
    const before = beforeQuote ? findQuoteRange(root, [beforeQuote]) : null;
    const after = afterQuote ? findQuoteRange(root, [afterQuote]) : null;
    if ((beforeQuote && !before) || (afterQuote && !after) || (!before && !after)) return NOT_FOUND;
    const from = before?.cloneRange() || document.createRange();
    const to = after?.cloneRange() || document.createRange();
    if (before) from.collapse(false);
    else {
      from.selectNodeContents(root);
      from.collapse(true);
    }
    if (after) to.collapse(true);
    else {
      to.selectNodeContents(root);
      to.collapse(false);
    }
    // chunk 可能恰好结束于图片之后。此时用下一个非空原文段落作为边界，
    // 而不是文档末尾。
    for (const block of blocks) {
      if (!block.textContent?.trim()) continue;
      const range = document.createRange();
      range.selectNodeContents(block);
      if (!after && range.compareBoundaryPoints(Range.START_TO_START, from) >= 0) {
        to.setStartBefore(block);
        to.collapse(true);
        break;
      }
      if (!before && range.compareBoundaryPoints(Range.END_TO_END, to) <= 0) {
        from.setStartAfter(block);
        from.collapse(true);
      }
    }
    const images = [...root.querySelectorAll('img')].filter((image) => {
      const range = document.createRange();
      range.selectNode(image);
      return (
        range.compareBoundaryPoints(Range.START_TO_START, from) >= 0 &&
        range.compareBoundaryPoints(Range.END_TO_END, to) <= 0
      );
    });
    if (images.length !== 1) return NOT_FOUND;
    commitMarks([], images);
    reveal(root, images[0]);
    return { found: true, precise: false, granularity: 'block' };
  }
  const excerpt = request.quotes.length === 1 ? request.quotes[0] : '';
  if (excerpt && request.scope && excerpt !== request.scope && findInText(request.scope, excerpt)) {
    const range = findQuoteRange(root, [excerpt]);
    if (range) {
      commitMarks([range], []);
      reveal(root, range);
      return { found: true, precise: true };
    }
  }
  const ranges: Range[] = [];
  let missed = false;
  let partial = false;
  let narrowed = false;
  for (const loc of request.locators.filter((l) => l.type === 'docx' && l.mapping === 'exact' && l.block)) {
    const el = blocks[(loc.block as number) - 1];
    const quote = sourceQuoteText(loc.quote || '');
    const sentence = request.sentence && findInText(quote, request.sentence) ? request.sentence : '';
    // 分页可能把 Word 段落拆开。接受偏移后的块之前，必须在全局重新校验完整引文，
    // 并拒绝重复匹配。
    const region = quote ? findQuoteRange(el, [quote]) || findQuoteRange(root, [quote]) : null;
    if (!region) {
      missed = true;
      continue;
    }
    const range = sentence
      ? findQuoteRange(
          region.commonAncestorContainer.nodeType === Node.TEXT_NODE
            ? region.commonAncestorContainer.parentElement
            : region.commonAncestorContainer,
          [sentence],
        )
      : region;
    ranges.push(range || region);
    narrowed ||= !!sentence && !!range;
    partial ||= !!loc.partial;
  }
  if (!ranges.length) return locateQuote(root, root, evidenceQuotes(request));
  const complete = !missed && (!partial || narrowed || !!findQuoteRange(root, evidenceQuotes(request)));
  commitMarks(ranges, []);
  reveal(root, ranges[0]);
  return { found: true, precise: complete, ...(!complete ? { reason: 'partial' } : {}) };
}

function locatePptx(request: SourceLocateRequest): SourceLocateResult {
  const box = previewContent.value;
  if (!box) return NOT_FOUND;
  const slides = Array.from(box.querySelectorAll('.pptx-preview-slide-wrapper'));
  const locators = request.locators.filter((l) => l.type === 'slide' && l.mapping === 'exact' && !l.partial && l.slide);
  if (!locators.length) return locateQuote(box, box, evidenceQuotes(request));
  const ranges: Range[] = [];
  const regions: Element[] = [];
  for (const loc of locators) {
    const slide = slides[(loc.slide as number) - 1];
    if (!slide) return NOT_FOUND;
    const own = slide.querySelector('.slide-wrapper') || slide;
    const range = loc.quote ? findQuoteRange(own, [loc.quote]) : null;
    if (range) ranges.push(range);
    else regions.push(slide);
  }
  commitMarks(ranges, regions);
  reveal(box, ranges[0] || regions[0]);
  return { found: true, precise: !regions.length, granularity: regions.length ? 'block' : 'text' };
}

/** 根据 SheetJS 输出的单元格 id 把工作表行号映射到表格行。 */
function sheetRows(table: Element): Map<number, Element> {
  const rows = new Map<number, Element>();
  for (const cell of Array.from(table.querySelectorAll('td[id]'))) {
    const match = /-[A-Z]+(\d+)$/.exec(cell.id);
    const tr = cell.closest('tr');
    if (match && tr && !rows.has(Number(match[1]))) rows.set(Number(match[1]), tr);
  }
  return rows;
}

function locateExcel(request: SourceLocateRequest): SourceLocateResult {
  const box = previewContent.value;
  if (!box) return NOT_FOUND;
  const locators = request.locators.filter((l) => l.type === 'sheet' && l.mapping === 'exact' && !l.partial && l.row_start);
  const targets: Element[] = [];
  for (const loc of locators) {
    const byName = loc.sheet ? excelSheetNames.value.indexOf(loc.sheet) : -1;
    if (byName < 0) continue;
    const sheetIdx = byName;
    const table = box.querySelector(`#user-content-sheet-${sheetIdx}`) || box.querySelector(`#sheet-${sheetIdx}`);
    if (!table) continue;
    const rows = sheetRows(table);
    const last = Math.min(loc.row_end || loc.row_start || 0, (loc.row_start || 0) + 200);
    for (let r = loc.row_start as number; r <= last; r++) {
      const tr = rows.get(r);
      if (tr && !targets.includes(tr)) targets.push(tr);
    }
  }
  if (targets.length) {
    commitMarks([], targets);
    reveal(box, targets[0]);
    return { found: true, precise: false, granularity: 'block' };
  }
  return locateQuote(box, box, evidenceQuotes(request));
}

function locateText(request: SourceLocateRequest): SourceLocateResult {
  const box = previewContent.value;
  const code = box?.querySelector('code') || box;
  if (!box || !code) return NOT_FOUND;
  // 用完整引文校验存储的偏移量。手工编辑或格式化的 JSON 文档
  // 不能复用其他版本的偏移。
  const locators = request.locators.filter((l) => l.type === 'text' && !l.partial && (l.end || 0) > (l.start || 0));
  if (locators.length && previewType.value === 'text' && !shouldPrettyPrintJson(previewExt.value)) {
    const ranges = locators.map((l) => rangeForOffsets(code, l.start || 0, l.end || 0));
    if (ranges.every((r, i) => r && locators[i].quote && findInText(r.toString(), locators[i].quote!))) {
      commitMarks(ranges as Range[], []);
      reveal(box, ranges[0]);
      return { found: true, precise: true };
    }
  }
  return locateQuote(code, box, evidenceQuotes(request));
}

function locateMarkdown(request: SourceLocateRequest): SourceLocateResult {
  const box = previewContent.value;
  if (!box) return NOT_FOUND;
  const markdown = request.sourceMarkdown ?? request.scope ?? request.quotes[0] ?? '';
  const match = findMarkdownSourceRange(box, markdown, request.sentence);
  if (!match) return NOT_FOUND;
  commitMarks(match.ranges || [match.range], []);
  reveal(box, match.range);
  return { found: true, precise: match.exact, ...(!match.exact ? { granularity: 'block' } : {}) };
}

function locateImage(request: SourceLocateRequest): SourceLocateResult {
  const boxes = request.locators
    .filter((l) => l.type === 'pdf' && l.mapping === 'exact' && !l.partial && (l.page || 1) === 1 && l.bbox?.length === 4)
    .map((l) => {
      const [x0, y0, x1, y1] = l.bbox as number[];
      return { left: x0, top: y0, width: x1 - x0, height: y1 - y0 };
    });
  imageMarks.value = boxes;
  // 没有区域时，整张图片即为被引用原文。
  return { found: true, precise: false, granularity: 'block' };
}

function formatClock(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

function locateAudio(request: SourceLocateRequest): SourceLocateResult {
  const audio = previewContent.value as HTMLAudioElement | null;
  const loc = request.locators.find((l) => l.type === 'time' && (l.end_ms || 0) > (l.start_ms || 0));
  if (!audio || !loc) return NOT_FOUND;
  // 只跳转，不自动播放；是否播放由读者决定。
  const seek = () => {
    audio.currentTime = (loc.start_ms || 0) / 1000;
  };
  if (audio.readyState >= 1) {
    seek();
  } else {
    // 更新的定位会清除此监听，避免旧引用赢得跳转。
    audio.addEventListener('loadedmetadata', seek, { once: true });
    clearLocate.push(() => audio.removeEventListener('loadedmetadata', seek));
  }
  audioRange.value = `${formatClock(loc.start_ms || 0)} – ${formatClock(loc.end_ms || 0)}`;
  audioQuote.value = loc.quote || '';
  return { found: true, precise: true };
}

function applyLocate() {
  const request = props.locate;
  clearLocateMarks();
  // pdf.js 查看器自行定位，并通过 @located 上报。
  if (!request || previewType.value === 'pdf') return;
  if (request.unavailable) {
    emit('located', { ...NOT_FOUND, reason: 'unavailable' });
    return;
  }
  let result = NOT_FOUND;
  try {
    switch (previewType.value) {
      case 'docx':
        result = locateDocx(request);
        break;
      case 'pptx':
        result = locatePptx(request);
        break;
      case 'excel':
        result = locateExcel(request);
        break;
      case 'text':
      case 'html':
        result = locateText(request);
        break;
      case 'markdown':
        result = locateMarkdown(request);
        break;
      case 'image':
        result = locateImage(request);
        break;
      case 'audio':
        result = locateAudio(request);
        break;
    }
  } catch (err) {
    console.warn('Locating the citation failed:', err);
  }
  emit('located', result);
}

watch(
  () => [props.active, props.knowledgeId, props.sessionId, props.attachmentId, props.messageId, props.artifactIndex, props.sourceBlob, props.fileName, props.fileType],
  ([active]) => {
    if (active && getPreviewSourceKey()) {
      loadPreview();
    }
  },
  { immediate: true }
);

onUnmounted(() => {
  document.body.style.overflow = '';
  cleanup();
});
</script>

<template>
  <div class="document-preview" :class="{ 'is-fullscreen': isFullscreen, 'fill-height': fillHeight }">
    <!-- Toolbar：提供 toolbarTarget 时渲染到宿主头部。 -->
    <Teleport :to="toolbarTarget || 'body'" :disabled="isFullscreen || !toolbarTarget">
      <div class="preview-toolbar" :class="{ 'is-inline': toolbarTarget && !isFullscreen }" v-if="isFullscreen || (!loading && !error && previewType !== 'unsupported')">
        <t-space size="small">
          <t-tooltip
            v-if="previewType === 'html' && allowsHtmlScriptPreview()"
            :content="htmlViewMode === 'render' ? $t('preview.htmlSource') : $t('preview.htmlRendered')"
            placement="bottom"
          >
            <t-button theme="default" variant="text" shape="square" @click="htmlViewMode = htmlViewMode === 'render' ? 'source' : 'render'">
              <template #icon><t-icon :name="htmlViewMode === 'render' ? 'code' : 'browse'" /></template>
            </t-button>
          </t-tooltip>
          <t-tooltip :content="isFullscreen ? $t('preview.exitFullscreen') : $t('preview.fullscreen')" placement="bottom">
            <t-button theme="default" variant="text" shape="square" @click="toggleFullscreen">
              <template #icon><t-icon :name="isFullscreen ? 'fullscreen-exit' : 'fullscreen'" /></template>
            </t-button>
          </t-tooltip>
        </t-space>
      </div>
    </Teleport>

    <!-- Loading -->
    <div v-if="loading" class="preview-loading">
      <t-loading size="medium" />
      <span class="loading-text">{{ $t('preview.loading') }}</span>
    </div>

    <!-- Error -->
    <div v-else-if="error" class="preview-error">
      <t-icon name="error-circle" size="48px" />
      <p>{{ error }}</p>
      <t-button theme="primary" size="small" @click="loadedForId = ''; loadPreview()">
        {{ $t('preview.retry') }}
      </t-button>
    </div>

    <!-- Unsupported -->
    <div v-else-if="previewType === 'unsupported'" class="preview-unsupported">
      <t-icon name="file-unknown" size="48px" />
      <p>{{ $t('preview.unsupported') }}</p>
      <p class="unsupported-hint">{{ $t('preview.unsupportedHint') }}</p>
    </div>

    <!-- PDF：原文模式使用 pdf.js 以支持定位高亮。 -->
    <div v-else-if="previewType === 'pdf' && pdfData" class="preview-pdf preview-pdf--source">
      <PdfSourceViewer :data="pdfData" :file-name="fileName" :locate="locate" @located="(r) => emit('located', r)" />
    </div>
    <div v-else-if="previewType === 'pdf' && blobUrl" class="preview-pdf">
      <iframe ref="previewContent" tabindex="0" :title="fileName" :src="blobUrl" class="pdf-iframe" />
    </div>

    <!-- HTML: artifacts render in a unique-origin iframe; other sources stay as source. -->
    <div v-else-if="previewType === 'html'" class="preview-html">
      <iframe
        v-if="allowsHtmlScriptPreview() && htmlViewMode === 'render' && blobUrl"
        :src="blobUrl"
        class="html-iframe"
        sandbox="allow-scripts"
        referrerpolicy="no-referrer"
      />
      <pre v-show="!allowsHtmlScriptPreview() || htmlViewMode === 'source'" ref="previewContent" tabindex="0" :aria-label="fileName" class="code-preview"><code class="hljs" v-html="highlightedCode"></code></pre>
    </div>

    <!-- Image -->
    <div v-else-if="previewType === 'image' && blobUrl" ref="previewContent" tabindex="0" :aria-label="fileName" class="preview-image">
      <div class="image-wrapper">
        <div class="image-frame">
          <img :src="blobUrl" :alt="fileName" @load="onImageLoad" />
          <div
            v-for="(mark, i) in imageMarks"
            :key="i"
            class="image-locate-mark"
            aria-hidden="true"
            :style="{ left: `${mark.left * 100}%`, top: `${mark.top * 100}%`, width: `${mark.width * 100}%`, height: `${mark.height * 100}%` }"
          />
        </div>
        <div v-if="imageNaturalWidth" class="image-info">
          {{ imageNaturalWidth }} × {{ imageNaturalHeight }} px
        </div>
      </div>
    </div>

    <!-- DOCX -->
    <div v-else-if="previewType === 'docx'" class="preview-docx">
      <div ref="docxContainer" tabindex="0" :aria-label="fileName" class="docx-container" />
    </div>

    <!-- PPTX -->
    <div v-else-if="previewType === 'pptx' && pptxData" ref="previewContent" tabindex="0" :aria-label="fileName" class="preview-pptx">
      <vue-office-pptx :src="pptxData" @rendered="scheduleLocate" @error="(e: any) => { error = e?.message || $t('preview.loadFailed'); }" />
    </div>

    <!-- Excel -->
    <div v-else-if="previewType === 'excel' && excelHtml" class="preview-excel">
      <div ref="previewContent" tabindex="0" :aria-label="fileName" class="excel-container" v-html="excelHtml" />
    </div>

    <!-- Markdown -->
    <div v-else-if="previewType === 'markdown' && markdownHtml" ref="previewContent" tabindex="0" :aria-label="fileName" class="preview-markdown">
      <div class="markdown-body" v-html="markdownHtml" />
    </div>

    <!-- Text / Code -->
    <div v-else-if="previewType === 'text' && highlightedCode" class="preview-text">
      <pre ref="previewContent" tabindex="0" :aria-label="fileName" class="code-preview"><code class="hljs" v-html="highlightedCode"></code></pre>
    </div>

    <!-- Mermaid -->
    <div v-else-if="previewType === 'mermaid' && mermaidSvg" class="preview-mermaid" @click="openMermaid">
      <div class="mermaid-body" v-html="mermaidSvg" />
    </div>

    <!-- Audio -->
    <div v-else-if="previewType === 'audio' && blobUrl" class="preview-audio">
      <div class="audio-wrapper">
        <t-icon name="sound" size="48px" />
        <p class="audio-filename">{{ fileName }}</p>
        <audio ref="previewContent" controls :src="blobUrl" class="audio-element">
          {{ $t('preview.audioNotSupported') }}
        </audio>
        <div v-if="audioRange" class="audio-locate">
          <span class="audio-locate__time">{{ audioRange }}</span>
          <p v-if="audioQuote" class="audio-locate__quote">{{ audioQuote }}</p>
        </div>
      </div>
    </div>

    <!-- Video -->
    <div v-else-if="previewType === 'video' && blobUrl" class="preview-video">
      <video controls playsinline :src="blobUrl" class="video-element">
        {{ $t('preview.videoNotSupported') }}
      </video>
    </div>
  </div>
</template>

<style scoped lang="less">
// ── Design tokens ──
@border-color: var(--td-component-stroke);
@border-radius: 6px;
@bg-white: var(--td-bg-color-container);
@bg-subtle: var(--td-bg-color-container);
@bg-muted: var(--td-bg-color-secondarycontainer);
@text-primary: var(--td-text-color-primary);
@text-secondary: var(--td-text-color-secondary);
@text-tertiary: var(--td-text-color-placeholder);
@text-disabled: var(--td-text-color-disabled);
@accent: var(--td-brand-color);
@accent-hover: var(--td-brand-color-active);
@accent-bg: var(--td-success-color-light);
@accent-bg-hover: var(--td-success-color-light);
@error-color: var(--td-error-color);
@table-border: var(--td-component-stroke);
@preview-max-h: calc(100vh - 200px);
// Note: <html> carries a `zoom` multiplier for font-size control, so 100vh
// is evaluated against the unscaled viewport and the resulting max-height
// may exceed the real viewport by the zoom factor (≤12.5% at "large").
// That produces an extra bit of scroll inside the non-fullscreen preview,
// which is acceptable for document reading. Not worth the complexity of
// inverse-scaling here.
@transition: all 0.2s ease;

// ── Shared container mixin ──
.preview-container() {
  border: 1px solid @border-color;
  border-radius: @border-radius;
  overflow: auto;
  max-height: @preview-max-h;
  background: @bg-white;
}

.document-preview {
  min-height: 200px;
  position: relative;

  &.fill-height {
    height: 100%;
    min-height: 0;
    display: flex;
    flex-direction: column;

    .preview-pdf,
    .preview-pptx,
    .preview-docx,
    .preview-image,
    .preview-excel,
    .preview-markdown,
    .preview-text,
    .preview-html,
    .preview-mermaid,
    .preview-audio,
    .preview-video,
    .preview-loading,
    .preview-error,
    .preview-unsupported {
      flex: 1;
      min-height: 0;
      max-height: none;
    }

    .preview-pdf {
      height: auto;
      min-height: 0;
    }

    .preview-pptx {
      overflow: auto;
    }

    .preview-docx {
      display: flex;
      flex-direction: column;

      .docx-container {
        flex: 1;
        min-height: 0;
        max-height: none;
        height: auto;
      }
    }

    .preview-image .image-wrapper img {
      max-height: 100%;
    }

    .preview-excel .excel-container,
    .preview-markdown,
    .preview-text .code-preview,
    .preview-html .code-preview {
      max-height: none;
    }

    .preview-html .html-iframe {
      height: 100%;
    }

    .preview-html {
      height: auto;
      min-height: 0;
    }
  }
}

.is-fullscreen {
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 2001;
  background: var(--td-bg-color-container);
  padding: 0;
  overflow-y: auto;

  .preview-toolbar {
    position: fixed;
    top: 12px;
    right: 32px;
    z-index: 2002;
  }

  /* Children use height: 100% rather than 100vh because <html> carries a
     `zoom` multiplier for font-size control; 100vh resolves against the
     unscaled viewport and then gets scaled, overshooting the screen. The
     fullscreen container is already inset 0 on all sides, so 100% resolves
     to the true viewport height. */
  .preview-pdf {
    height: 100%;
  }

  .preview-pptx {
    height: auto;
    min-height: 100%;
    overflow: visible;
    border: none;

    :deep(.pptx-preview-wrapper) {
      height: auto !important;
      overflow-y: visible !important;
    }
  }

  .preview-docx {
    height: 100%;
    display: flex;
    flex-direction: column;
    .docx-container {
      max-height: 100%;
      height: 100%;
      flex: 1;
    }
  }

  .preview-image {
    min-height: 100%;
    display: flex;
    justify-content: center;
    align-items: center;
    .image-wrapper img {
      max-height: calc(100% - 80px);
    }
  }

  .preview-excel .excel-container,
  .preview-markdown,
  .preview-text .code-preview,
  .preview-html .code-preview {
    max-height: 100%;
  }

  .preview-html,
  .preview-video,
  .preview-mermaid {
    height: 100%;
  }
}

.preview-toolbar {
  position: absolute;
  top: 8px;
  right: 24px;
  z-index: 10;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-border);
  border-radius: var(--td-radius-default);
  box-shadow: var(--td-shadow-1);
  padding: 4px;
  opacity: 0.6;
  transition: opacity 0.2s;

  &:hover {
    opacity: 1;
  }

  // 渲染到宿主头部时不再浮动。
  &.is-inline {
    position: static;
    padding: 0;
    margin: 0;
    border: 0;
    background: transparent;
    box-shadow: none;
    opacity: 1;
  }
}

// ── States ──
.preview-loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 60px 20px;
  gap: 16px;
  .loading-text { color: @text-tertiary; font-size: 14px; }
}

.preview-error {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 60px 20px;
  gap: 12px;
  color: @error-color;
  p { margin: 0; font-size: 14px; color: @text-secondary; }
}

.preview-unsupported {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 60px 20px;
  gap: 12px;
  color: @text-disabled;
  p { margin: 0; font-size: 14px; color: @text-secondary; }
  .unsupported-hint { font-size: 12px; color: @text-tertiary; }
}

// ── PDF ──
.preview-pdf {
  width: 100%;
  height: @preview-max-h;
  min-height: 500px;
  .pdf-iframe {
    width: 100%;
    height: 100%;
    border: none;
    border-radius: @border-radius;
  }

  // pdf.js 查看器自行管理滚动区域。
  &--source {
    display: flex;
    flex-direction: column;
    overflow: hidden;
    border: 1px solid @border-color;
    border-radius: @border-radius;
  }
}

// ── HTML ──
.preview-html {
  width: 100%;
  height: @preview-max-h;
  min-height: 420px;
  display: flex;
  flex-direction: column;
  .html-iframe {
    flex: 1;
    width: 100%;
    min-height: 0;
    border: 1px solid @border-color;
    border-radius: @border-radius;
    background: @bg-white;
  }
  .code-preview {
    .preview-container();
    flex: 1;
    min-height: 0;
    margin: 0;
    padding: 16px;
    background: @bg-subtle;
    font-size: 13px;
    line-height: 1.6;
    code {
      white-space: pre;
      word-wrap: normal;
      display: block;
      background: transparent;
    }
  }
}

// ── Mermaid ──
.preview-mermaid {
  .preview-container();
  display: flex;
  justify-content: center;
  padding: 24px 16px;
  cursor: zoom-in;
  .mermaid-body {
    max-width: 100%;
    :deep(svg) {
      max-width: 100%;
      height: auto;
    }
  }
}

// ── Image ──
.preview-image {
  display: flex;
  justify-content: center;
  padding: 20px 0;
  .image-wrapper {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
    .image-frame {
      position: relative;
      display: inline-block;
      max-width: 100%;
    }
    img {
      max-width: 100%;
      max-height: calc(100vh - 280px);
      border-radius: @border-radius;
      box-shadow: 0 2px 12px rgba(7, 192, 95, 0.08);
      object-fit: contain;
      display: block;
    }
    .image-locate-mark {
      position: absolute;
      border: 1px solid var(--app-source-highlight-border);
      border-radius: var(--app-radius-xs);
      background: var(--app-source-highlight-soft-bg);
      pointer-events: none;
    }
    .image-info { font-size: 12px; color: @text-tertiary; }
  }
}

// ── Markdown ──
.preview-markdown {
  .preview-container();
  padding: 20px 24px;
}

// ── DOCX ──
.preview-docx {
  .docx-container { .preview-container(); }
}

// ── PPTX ──
.preview-pptx {
  max-height: @preview-max-h;
  min-height: 500px;
  border: 1px solid @border-color;
  border-radius: @border-radius;
  overflow: auto;
  background: @bg-subtle;

  :deep(.pptx-preview-wrapper) {
    height: auto !important;
    overflow-y: visible !important;
  }
}

// ── Excel ──
.preview-excel {
  .excel-container { .preview-container(); }
}

// ── Text / Code ──
.preview-text {
  .code-preview {
    .preview-container();
    margin: 0;
    padding: 16px;
    background: @bg-subtle;
    font-size: 13px;
    line-height: 1.6;
    code {
      white-space: pre;
      word-wrap: normal;
      display: block;
      background: transparent;
    }
  }
}

// ── Audio ──
.preview-audio {
  display: flex;
  justify-content: center;
  padding: 40px 20px;
  .audio-wrapper {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 16px;
    color: @text-secondary;
    .audio-filename { font-size: 14px; color: @text-primary; margin: 0; }
    .audio-element { width: 100%; max-width: 480px; }
    .audio-locate {
      width: 100%;
      max-width: 480px;
      padding: 10px 12px;
      border-left: 2px solid var(--app-source-highlight-border);
      background: var(--app-source-highlight-soft-bg);
      text-align: left;
    }
    .audio-locate__time {
      font-size: 12px;
      color: var(--app-source-highlight);
      font-variant-numeric: tabular-nums;
    }
    .audio-locate__quote {
      margin: 4px 0 0;
      font-size: 13px;
      line-height: 1.6;
      color: @text-primary;
      white-space: pre-wrap;
    }
  }
}

// ── Video ──
.preview-video {
  display: flex;
  justify-content: center;
  align-items: center;
  padding: 16px;
  min-height: 280px;
  .video-element {
    width: 100%;
    max-height: calc(100vh - 240px);
    border-radius: @border-radius;
    background: #000;
  }
}

// ── Deep styles (v-html / third-party components) ──

// Shared table mixin for v-html content
.preview-table() {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
  th, td {
    border: 1px solid @table-border;
    padding: 6px 12px;
    text-align: left;
  }
  th {
    background: @accent-bg;
    font-weight: 600;
    color: @text-primary;
  }
  tr:hover td {
    background: @accent-bg;
    transition: @transition;
  }
}

:deep(.markdown-body) {
  font-size: 14px;
  line-height: 1.7;
  color: @text-primary;
  word-break: break-word;

  h1, h2, h3, h4, h5, h6 {
    margin-top: 20px;
    margin-bottom: 10px;
    font-weight: 600;
    line-height: 1.4;
  }
  h1 { font-size: 24px; border-bottom: 1px solid @border-color; padding-bottom: 8px; }
  h2 { font-size: 20px; border-bottom: 1px solid @border-color; padding-bottom: 6px; }
  h3 { font-size: 17px; }

  p { margin: 8px 0; }
  blockquote {
    margin: 12px 0;
    padding: 8px 16px;
    border-left: 4px solid @accent;
    background: @bg-subtle;
    color: var(--td-text-color-secondary);
  }
  ul, ol { padding-left: 24px; margin: 8px 0; }
  li { margin: 4px 0; }

  table { .preview-table(); margin: 12px 0; }

  pre {
    margin: 12px 0;
    padding: 14px;
    background: @bg-subtle;
    border-radius: @border-radius;
    overflow: auto;
    font-size: 13px;
    line-height: 1.5;
    code { background: transparent; padding: 0; }
  }
  code {
    background: var(--td-bg-color-secondarycontainer);
    padding: 2px 6px;
    border-radius: 3px;
    font-size: 0.9em;
  }
  img { max-width: 100%; border-radius: 4px; }
  hr { border: none; border-top: 1px solid @border-color; margin: 20px 0; }
  a { color: @accent; text-decoration: none; &:hover { color: @accent-hover; text-decoration: underline; } }
  strong { font-weight: 600; }
}

:deep(.docx-preview-wrapper) {
  padding: 20px;
  max-width: 100%;
  width: 100%;
  box-sizing: border-box;
  overflow-x: auto; // 如果内容过宽，允许水平滚动而不是溢出
  
  // 约束所有子元素的宽度
  * {
    max-width: 100%;
    box-sizing: border-box;
  }
  
  // 特别处理表格
  table {
    width: 100%;
    table-layout: auto;
    word-wrap: break-word;
  }
  
  // 处理图片
  img {
    max-width: 100%;
    height: auto;
  }
  
  // 处理可能的固定宽度元素
  [style*="width"] {
    max-width: 100% !important;
  }
}

:deep(.vue-office-pptx) {
  width: 100%;
  min-height: 100%;
}

:deep(.vue-office-pptx-main) {
  width: 100%;
  min-height: 100%;
}

:deep(.excel-sheet) {
  padding: 0;
  .excel-sheet-name {
    position: sticky;
    top: 0;
    background: @accent-bg;
    padding: 8px 16px;
    font-weight: 600;
    font-size: 13px;
    color: @text-primary;
    border-bottom: 1px solid @border-color;
    z-index: 1;
  }
  table {
    .preview-table();
    th, td {
      white-space: nowrap;
      max-width: 300px;
      overflow: hidden;
      text-overflow: ellipsis;
    }
  }
}
</style>

<!-- highlight.js github.css is a light theme imported globally; its token
     colors (notably the base #24292e) become unreadable on the dark
     container background used in dark mode. Remap the palette to the
     github-dark colors when dark mode is active. Non-scoped on purpose so
     it covers every hljs block (txt/code preview, markdown code fences). -->
<style lang="less">
html[theme-mode="dark"] {
  .hljs {
    color: #c9d1d9;
    background: transparent;
  }
  .hljs-doctag,
  .hljs-keyword,
  .hljs-meta .hljs-keyword,
  .hljs-template-tag,
  .hljs-template-variable,
  .hljs-type,
  .hljs-variable.language_ {
    color: #ff7b72;
  }
  .hljs-title,
  .hljs-title.class_,
  .hljs-title.class_.inherited__,
  .hljs-title.function_ {
    color: #d2a8ff;
  }
  .hljs-attr,
  .hljs-attribute,
  .hljs-literal,
  .hljs-meta,
  .hljs-number,
  .hljs-operator,
  .hljs-variable,
  .hljs-selector-attr,
  .hljs-selector-class,
  .hljs-selector-id {
    color: #79c0ff;
  }
  .hljs-regexp,
  .hljs-string,
  .hljs-meta .hljs-string {
    color: #a5d6ff;
  }
  .hljs-built_in,
  .hljs-symbol {
    color: #ffa657;
  }
  .hljs-comment,
  .hljs-code,
  .hljs-formula {
    color: #8b949e;
  }
  .hljs-name,
  .hljs-quote,
  .hljs-selector-tag,
  .hljs-selector-pseudo {
    color: #7ee787;
  }
  .hljs-subst {
    color: #c9d1d9;
  }
  .hljs-section {
    color: #1f6feb;
    font-weight: bold;
  }
  .hljs-bullet {
    color: #f2cc60;
  }
  .hljs-emphasis {
    color: #c9d1d9;
    font-style: italic;
  }
  .hljs-strong {
    color: #c9d1d9;
    font-weight: bold;
  }
  .hljs-addition {
    color: #aff5b4;
    background-color: #033a16;
  }
  .hljs-deletion {
    color: #ffdcd7;
    background-color: #67060c;
  }
}
</style>

<style lang="less">
/* 引用高亮不放在 scoped 块中：它们作用于第三方渲染器
   （docx-preview、pptx-preview、SheetJS）创建出来的 DOM。 */
::highlight(source-locate) {
  background-color: var(--app-source-highlight-bg);
}

mark.source-locate-mark {
  background-color: var(--app-source-highlight-bg);
  color: inherit;
  border-radius: var(--app-radius-xs);
  box-decoration-break: clone;
  -webkit-box-decoration-break: clone;
}

.source-locate-block {
  background-color: var(--app-source-highlight-soft-bg) !important;
  box-shadow: inset 3px 0 var(--app-source-highlight-border);
}

/* 单元格背景可能遮住行标记。每个单元格只绘制一次，
   强调色保留在起始边缘，避免给每一行都加边框。 */
tr.source-locate-block {
  background-color: transparent !important;
  box-shadow: none;
}

tr.source-locate-block > td {
  background-color: var(--app-source-highlight-soft-bg) !important;
}

tr.source-locate-block > td:first-child {
  box-shadow: inset 3px 0 var(--app-source-highlight-border);
}

img.source-locate-block {
  outline: 1px solid var(--app-source-highlight-border);
  outline-offset: 2px;
}
</style>
