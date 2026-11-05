/**
 * Map file extensions to display categories used by the artifact library
 * and the in-chat artifact list. The categories drive both the icon and
 * the filter tabs.
 */

export type ArtifactFileType =
  | 'document'
  | 'spreadsheet'
  | 'presentation'
  | 'image'
  | 'audio'
  | 'video'
  | 'archive'
  | 'code'
  | 'data'
  | 'other';

const DOCUMENT_EXT = new Set([
  '.pdf',
  '.doc',
  '.docx',
  '.odt',
  '.rtf',
  '.txt',
  '.md',
  '.markdown',
  '.rst',
]);

const SPREADSHEET_EXT = new Set([
  '.xls',
  '.xlsx',
  '.csv',
  '.tsv',
  '.ods',
  '.numbers',
]);

const PRESENTATION_EXT = new Set([
  '.ppt',
  '.pptx',
  '.odp',
  '.key',
]);

const IMAGE_EXT = new Set([
  '.png',
  '.jpg',
  '.jpeg',
  '.gif',
  '.webp',
  '.svg',
  '.bmp',
  '.ico',
  '.tiff',
  '.tif',
  '.avif',
]);

const AUDIO_EXT = new Set([
  '.mp3',
  '.wav',
  '.ogg',
  '.flac',
  '.aac',
  '.m4a',
  '.wma',
]);

const VIDEO_EXT = new Set([
  '.mp4',
  '.webm',
  '.mov',
  '.avi',
  '.mkv',
  '.flv',
  '.wmv',
  '.m4v',
]);

const ARCHIVE_EXT = new Set([
  '.zip',
  '.tar',
  '.gz',
  '.tgz',
  '.bz2',
  '.7z',
  '.rar',
  '.xz',
]);

const CODE_EXT = new Set([
  '.js',
  '.ts',
  '.jsx',
  '.tsx',
  '.py',
  '.java',
  '.c',
  '.cpp',
  '.cc',
  '.h',
  '.hpp',
  '.go',
  '.rs',
  '.rb',
  '.php',
  '.sh',
  '.bash',
  '.zsh',
  '.sql',
  '.html',
  '.htm',
  '.css',
  '.scss',
  '.less',
  '.vue',
  '.swift',
  '.kt',
  '.dart',
  '.lua',
  '.r',
  '.m',
  '.mm',
]);

const DATA_EXT = new Set([
  '.json',
  '.xml',
  '.yaml',
  '.yml',
  '.toml',
  '.ini',
  '.parquet',
  '.feather',
]);

export function classifyArtifactFile(fileName: string): ArtifactFileType {
  const ext = fileName.toLowerCase().substring(fileName.lastIndexOf('.'));
  if (!ext || ext === fileName) return 'other';
  if (DOCUMENT_EXT.has(ext)) return 'document';
  if (SPREADSHEET_EXT.has(ext)) return 'spreadsheet';
  if (PRESENTATION_EXT.has(ext)) return 'presentation';
  if (IMAGE_EXT.has(ext)) return 'image';
  if (AUDIO_EXT.has(ext)) return 'audio';
  if (VIDEO_EXT.has(ext)) return 'video';
  if (ARCHIVE_EXT.has(ext)) return 'archive';
  if (CODE_EXT.has(ext)) return 'code';
  if (DATA_EXT.has(ext)) return 'data';
  return 'other';
}

/**
 * TDesign icon name for each file category.
 */
export function artifactFileIcon(type: ArtifactFileType): string {
  switch (type) {
    case 'document':
      return 'file-copy';
    case 'spreadsheet':
      return 'chart-column';
    case 'presentation':
      return 'slideshow';
    case 'image':
      return 'image';
    case 'audio':
      return 'music';
    case 'video':
      return 'video';
    case 'archive':
      return 'archive';
    case 'code':
      return 'code';
    case 'data':
      return 'database';
    default:
      return 'file';
  }
}
