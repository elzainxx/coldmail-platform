// The line a verification's preview says about the outside service some addresses may be checked with (the paid lookup after our own checks). The wording is a
// PLACEHOLDER: the customer text is drafted with the terms in Phase 8, and lives in copy/newjob.ts under `newjob.disclosure.*`, so only that file changes then.
//
// It says what is true of THIS job: a job keeps the setting it was made with (Docs/jobs-api.md, "Settings"), so the line follows the job, and when the user has
// changed the setting since (the settings page, GET /v1/settings) a second sentence says the new one applies to the next job, not to this one.
import { t } from '../../copy/index.ts'
import { Link } from '../../router.tsx'

export interface VendorDisclosureProps {
  /** the opt-out this job was made with */
  jobOptedOut: boolean
  /** the opt-out in force now, or null when the settings could not be read */
  settingsOptedOut: boolean | null
}

export function VendorDisclosure({ jobOptedOut, settingsOptedOut }: VendorDisclosureProps) {
  return (
    <div className="rounded-md bg-sunken px-3 py-2 text-sm text-fg">
      <p>
        {jobOptedOut ? t('newjob.disclosure.vendorOptedOut') : t('newjob.disclosure.vendor')} <Link to="/settings">{t('newjob.disclosure.settingsLink')}</Link>
      </p>
      {settingsOptedOut !== null && settingsOptedOut !== jobOptedOut && <p className="mt-1 text-fg-muted">{t('newjob.disclosure.changedSince')}</p>}
    </div>
  )
}
