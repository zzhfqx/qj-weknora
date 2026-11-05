import type { ArtifactLibraryItem } from '@/api/artifacts';

/**
 * Human-readable file size, e.g. "1.2 MB".
 */
export function formatArtifactSize(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unitIdx = 0;
  while (value >= 1024 && unitIdx < units.length - 1) {
    value /= 1024;
    unitIdx++;
  }
  const precision = unitIdx === 0 ? 0 : value < 10 ? 2 : 1;
  return `${value.toFixed(precision)} ${units[unitIdx]}`;
}

/**
 * Format an artifact timestamp (RFC3339 string) for display.
 * Today → HH:mm, yesterday → 昨天 HH:mm, older → YYYY-MM-DD.
 */
export function formatArtifactDateTime(iso: string | number | Date | undefined): string {
  if (!iso) return '';
  const date = typeof iso === 'string' || typeof iso === 'number' ? new Date(iso) : iso;
  if (Number.isNaN(date.getTime())) return '';
  const now = new Date();
  const isSameDay =
    date.getFullYear() === now.getFullYear() &&
    date.getMonth() === now.getMonth() &&
    date.getDate() === now.getDate();
  if (isSameDay) {
    return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  const isYesterday =
    date.getFullYear() === yesterday.getFullYear() &&
    date.getMonth() === yesterday.getMonth() &&
    date.getDate() === yesterday.getDate();
  const timeStr = date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (isYesterday) {
    return `昨天 ${timeStr}`;
  }
  const sameYear = date.getFullYear() === now.getFullYear();
  if (sameYear) {
    return `${date.getMonth() + 1}月${date.getDate()}日`;
  }
  return `${date.getFullYear()}/${date.getMonth() + 1}/${date.getDate()}`;
}

/**
 * Build a download URL for a library item. The download goes through the
 * per-session endpoint so ownership is re-checked server-side.
 */
export function artifactDownloadUrl(item: ArtifactLibraryItem): string {
  return `/api/v1/sessions/${item.session_id}/messages/${item.message_id}/artifacts/${item.index}/download`;
}
