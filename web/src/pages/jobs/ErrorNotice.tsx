import type { ApiError } from '../../api/errors.ts'
import { errorReference, errorText, t } from '../../copy/index.ts'
import { Button } from '../../ui/index.ts'

export interface ErrorNoticeProps {
  error: ApiError
  /** a Try again button: shown when the failed request can simply be made again */
  onRetry?: () => void
}

/** What a failed request says: the one line of the catalogue for its code (never the server's sentence or the code) and the reference to give to support. */
export function ErrorNotice({ error, onRetry }: ErrorNoticeProps) {
  const reference = errorReference(error)
  return (
    <div role="alert" className="flex flex-col gap-2 rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
      <p>{errorText(error)}</p>
      {reference !== null && <p className="text-xs">{reference}</p>}
      {onRetry !== undefined && (
        <div>
          <Button size="sm" onClick={onRetry}>
            {t('common.retry')}
          </Button>
        </div>
      )}
    </div>
  )
}
