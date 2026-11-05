import { del, get } from './request';

export interface ArtifactLibraryItem {
  session_id: string;
  message_id: string;
  index: number;
  file_name: string;
  file_type: string;
  file_size: number;
  source_path: string;
  /** RFC3339 string — latest version's creation time */
  created_at: string;
  /** RFC3339 string — latest version's sandbox mod_time */
  mod_time: string;
  version_count: number;
}

export interface ArtifactLibraryQuery {
  keyword?: string;
  file_types?: string[];
  page?: number;
  page_size?: number;
}

export interface ArtifactLibraryResponse {
  data: ArtifactLibraryItem[];
  total: number;
  page: number;
  page_size: number;
}

// listArtifactLibrary returns a paginated list of artifacts across all of
// the caller's own sessions (web bucket), one row per source file (latest
// version only).
export async function listArtifactLibrary(
  query: ArtifactLibraryQuery = {},
): Promise<ArtifactLibraryResponse> {
  const params: Record<string, unknown> = {};
  if (query.keyword) params.keyword = query.keyword;
  if (query.file_types?.length) params.file_types = query.file_types.join(',');
  if (query.page) params.page = query.page;
  if (query.page_size) params.page_size = query.page_size;
  return get('/api/v1/artifacts', { params });
}

// deleteLibraryArtifact deletes one artifact from the library. The library
// entry represents a file across versions, so all_versions defaults to true.
export async function deleteLibraryArtifact(params: {
  session_id: string;
  message_id: string;
  index: number;
  all_versions?: boolean;
}) {
  return del('/api/v1/artifacts', { params });
}
