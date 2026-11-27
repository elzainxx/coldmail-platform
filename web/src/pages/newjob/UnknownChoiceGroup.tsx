// What a send to a finished verification does with the addresses that came back "unknown": the user MUST say, there is no default (Docs/jobs-api.md, "The
// `unknown` choice of a chained send is never defaulted"). Two radio buttons, neither chosen until the user chooses.
import { useId } from 'react'
import type { UnknownChoice } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Choice, ChoiceGroup } from './Choice.tsx'

export interface UnknownChoiceGroupProps {
  value: UnknownChoice | null
  onChange: (choice: UnknownChoice) => void
  /** the sentence that says the question is still open */
  error?: string | null
  disabled?: boolean
}

export function UnknownChoiceGroup({ value, onChange, error, disabled = false }: UnknownChoiceGroupProps) {
  const name = useId()
  return (
    <ChoiceGroup legend={t('newjob.unknown.legend')} hint={t('newjob.unknown.hint')} error={error}>
      <Choice name={name} value="include" checked={value === 'include'} disabled={disabled} onChange={() => onChange('include')} label={t('newjob.unknown.include')} />
      <Choice name={name} value="exclude" checked={value === 'exclude'} disabled={disabled} onChange={() => onChange('exclude')} label={t('newjob.unknown.exclude')} />
    </ChoiceGroup>
  )
}
