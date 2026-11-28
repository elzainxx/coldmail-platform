// Step 2 of a send: the list. A CSV file (the same file picker and column picker as a verification), or the results of a verification the user already finished.
import type { Job, UnknownChoice } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Choice, ChoiceGroup } from './Choice.tsx'
import { CsvStep } from './CsvStep.tsx'
import type { CsvSource } from './useCsvSource.ts'
import { VerificationSource } from './VerificationSource.tsx'

export type ListKind = 'csv' | 'verification'

export interface ListStepProps {
  kind: ListKind
  onKind: (kind: ListKind) => void
  csv: CsvSource
  verification: Job | null
  onVerification: (job: Job) => void
  unknownChoice: UnknownChoice | null
  onUnknownChoice: (choice: UnknownChoice) => void
}

export function ListStep({ kind, onKind, csv, verification, onVerification, unknownChoice, onUnknownChoice }: ListStepProps) {
  return (
    <div className="flex flex-col gap-4">
      <ChoiceGroup legend={t('newjob.list.legend')}>
        <Choice name="list-kind" value="csv" checked={kind === 'csv'} onChange={() => onKind('csv')} label={t('newjob.list.csv')} description={t('newjob.list.csvDescription')} />
        <Choice
          name="list-kind"
          value="verification"
          checked={kind === 'verification'}
          onChange={() => onKind('verification')}
          label={t('newjob.list.verification')}
          description={t('newjob.list.verificationDescription')}
        />
      </ChoiceGroup>
      {kind === 'csv' ? (
        <CsvStep source={csv} purpose="send" />
      ) : (
        <VerificationSource
          selected={verification}
          onSelect={onVerification}
          unknownChoice={unknownChoice}
          onUnknownChoice={onUnknownChoice}
        />
      )}
    </div>
  )
}
