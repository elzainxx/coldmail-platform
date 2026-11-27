// Choosing the list: the browser's own file button (a native input: the keyboard, the screen reader and the platform's file dialog all come with it) inside a
// box a file can also be dropped on. Nothing here reads the file; it only says which one was chosen.
import { useState, type ChangeEvent, type DragEvent } from 'react'
import { t } from '../../copy/index.ts'
import { cx, FormField, useControlA11y } from '../../ui/index.ts'

function FileInput({ onFile, disabled }: { onFile: (file: File) => void; disabled: boolean }) {
  const a11y = useControlA11y({})
  const change = (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    // cleared at once, so choosing the same file again (after fixing it in a spreadsheet) is a change the browser reports
    e.target.value = ''
    if (file !== undefined) onFile(file)
  }
  return (
    <input
      {...a11y}
      type="file"
      accept=".csv,text/csv"
      disabled={disabled}
      onChange={change}
      className="block w-full cursor-pointer text-base text-fg file:mr-3 file:h-8 file:cursor-pointer file:rounded-md file:border-0 file:bg-accent file:px-3 file:text-sm file:font-medium file:text-on-accent hover:file:bg-accent-hover disabled:cursor-not-allowed disabled:opacity-60"
    />
  )
}

export interface FilePickerProps {
  onFile: (file: File) => void
  disabled?: boolean
}

export function FilePicker({ onFile, disabled = false }: FilePickerProps) {
  const [dragging, setDragging] = useState(false)

  const over = (e: DragEvent<HTMLDivElement>) => {
    e.preventDefault()
    if (!disabled) setDragging(true)
  }
  const drop = (e: DragEvent<HTMLDivElement>) => {
    e.preventDefault()
    setDragging(false)
    const file = e.dataTransfer.files?.[0]
    if (!disabled && file !== undefined) onFile(file)
  }

  return (
    <div
      onDragOver={over}
      onDragEnter={over}
      onDragLeave={() => setDragging(false)}
      onDrop={drop}
      className={cx('rounded-lg border-2 border-dashed p-4', dragging ? 'border-accent bg-hover' : 'border-border-strong bg-sunken')}
    >
      <FormField label={t('newjob.csv.file')} hint={t('newjob.csv.fileHint')}>
        <FileInput onFile={onFile} disabled={disabled} />
      </FormField>
      <p className="mt-2 text-sm text-fg-muted">{dragging ? t('newjob.csv.dropActive') : t('newjob.csv.dropHint')}</p>
    </div>
  )
}
