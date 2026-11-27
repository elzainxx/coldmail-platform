// One typed function per route, so a screen never writes a path or guesses a shape. Each is a thin call of `api` (client.ts) with the types of types.ts.
// A route that is built by another agent in this phase (the "New API" table of the spec) is marked: if its real answer differs, fix types.ts and this file together.
import { api, type RequestOptions } from './client.ts'
import type {
  AddSuppressionResponse,
  Balances,
  ConfirmRequest,
  ConfirmResult,
  CreateJobCsvQuery,
  CreateJobJson,
  EventsPage,
  JobEnvelope,
  JobList,
  ListJobsQuery,
  M3AccountResponse,
  M3AccountsPage,
  M3BulkResponse,
  M3NewAccount,
  M3RemoveResponse,
  M3TestRequest,
  M3TestResponse,
  Me,
  PreviewRequest,
  RemoveSuppressionResponse,
  ResultsPage,
  SuppressionImportResult,
  SuppressionsPage,
  SuppressionsQuery,
  TemplateCheck,
  TemplateCheckRequest,
  UserSettings,
} from './types.ts'

type Opts = Pick<RequestOptions, 'signal'>

const enc = encodeURIComponent
const jobPath = (id: string) => `/v1/jobs/${enc(id)}`

// ---- jobs (Docs/jobs-api.md) ----

export const jobsApi = {
  list: (query: ListJobsQuery = {}, o: Opts = {}) => api.get<JobList>('/v1/jobs', { ...o, query: { ...query } }),
  get: (id: string, o: Opts = {}) => api.get<JobEnvelope>(jobPath(id), o),
  /** a draft from a JSON list */
  createJson: (body: CreateJobJson, o: Opts = {}) => api.post<JobEnvelope>('/v1/jobs', body, o),
  /** a draft from a CSV: pass the File itself, it is streamed from disk */
  createCsv: (file: Blob, query: CreateJobCsvQuery, o: Opts = {}) =>
    api.post<JobEnvelope>('/v1/jobs', file, { ...o, query: { ...query }, contentType: 'text/csv' }),
  preview: (id: string, body?: PreviewRequest, o: Opts = {}) => api.post<JobEnvelope>(`${jobPath(id)}/preview`, body, o),
  confirm: (id: string, body: ConfirmRequest, idempotencyKey: string, o: Opts = {}) =>
    api.post<ConfirmResult>(`${jobPath(id)}/confirm`, body, { ...o, idempotencyKey }),
  discard: (id: string, o: Opts = {}) => api.post<JobEnvelope>(`${jobPath(id)}/discard`, undefined, o),
  reopen: (id: string, o: Opts = {}) => api.post<JobEnvelope>(`${jobPath(id)}/reopen`, undefined, o),
  cancel: (id: string, o: Opts = {}) => api.post<JobEnvelope>(`${jobPath(id)}/cancel`, undefined, o),
  pause: (id: string, o: Opts = {}) => api.post<JobEnvelope>(`${jobPath(id)}/pause`, undefined, o),
  resume: (id: string, o: Opts = {}) => api.post<JobEnvelope>(`${jobPath(id)}/resume`, undefined, o),
  events: (id: string, query: { after?: number; limit?: number } = {}, o: Opts = {}) =>
    api.get<EventsPage>(`${jobPath(id)}/events`, { ...o, query: { ...query } }),
  results: (id: string, query: { cursor?: string; limit?: number } = {}, o: Opts = {}) =>
    api.get<ResultsPage>(`${jobPath(id)}/results`, { ...o, query: { ...query } }),
  /** the whole results CSV, as a Blob to hand to saveBlob (api/download.ts) */
  resultsCsv: (id: string, o: Opts = {}) => api.getBlob(`${jobPath(id)}/results.csv`, o),
}

export const settingsApi = {
  get: (o: Opts = {}) => api.get<UserSettings>('/v1/settings', o),
  put: (settings: UserSettings, o: Opts = {}) => api.put<UserSettings>('/v1/settings', settings, o),
}

// ---- Phase 6 additions (spec, "New API") ----

export const meApi = {
  get: (o: Opts = {}) => api.get<Me>('/v1/me', o),
}

export const balancesApi = {
  get: (o: Opts = {}) => api.get<Balances>('/v1/balances', o),
}

export const templatesApi = {
  check: (body: TemplateCheckRequest, o: Opts = {}) => api.post<TemplateCheck>('/v1/templates/check', body, o),
}

export const suppressionsApi = {
  list: (query: SuppressionsQuery = {}, o: Opts = {}) => api.get<SuppressionsPage>('/v1/suppressions', { ...o, query: { ...query } }),
  add: (email: string, o: Opts = {}) => api.post<AddSuppressionResponse>('/v1/suppressions', { email }, o),
  /** removal is a POST: an address does not belong in a path (spec, decision 8) */
  remove: (email: string, o: Opts = {}) => api.post<RemoveSuppressionResponse>('/v1/suppressions/remove', { email }, o),
  /** a CSV with an `email` column; pass the File */
  importCsv: (file: Blob, o: Opts = {}) => api.post<SuppressionImportResult>('/v1/suppressions/import', file, { ...o, contentType: 'text/csv' }),
}

export const m3Api = {
  list: (query: { cursor?: string } = {}, o: Opts = {}) => api.get<M3AccountsPage>('/v1/m3/accounts', { ...o, query: { ...query } }),
  add: (account: M3NewAccount, o: Opts = {}) => api.post<M3AccountResponse>('/v1/m3/accounts', account, o),
  test: (id: string, body: M3TestRequest, o: Opts = {}) => api.post<M3TestResponse>(`/v1/m3/accounts/${enc(id)}/test`, body, o),
  bulkJson: (accounts: M3NewAccount[], o: Opts = {}) => api.post<M3BulkResponse>('/v1/m3/accounts/bulk', { accounts }, o),
  bulkCsv: (file: Blob, o: Opts = {}) => api.post<M3BulkResponse>('/v1/m3/accounts/bulk', file, { ...o, contentType: 'text/csv' }),
  remove: (id: string, o: Opts = {}) => api.post<M3RemoveResponse>(`/v1/m3/accounts/${enc(id)}/remove`, undefined, o),
}
