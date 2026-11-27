// The list the user chose as a CSV file, as the screens need it: the file (kept, to be uploaded as it is), what reading its start found (csv/reader.ts),
// which column is the address and what, if anything, is wrong with the file. Choosing another file stops the reading of the previous one.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { csvProblems, suggestEmailColumn, type CsvProblem } from './csv/columns.ts'
import { readCsvHead, type CsvHead } from './csv/reader.ts'
import { isAbort } from './errors.ts'

export interface CsvSource {
  file: File | null
  head: CsvHead | null
  reading: boolean
  /** the file could not be read at all */
  readFailed: boolean
  /** the index of the address column, null until there is one */
  chosen: number | null
  problems: CsvProblem[]
  /** a file was read, an address column is chosen and the platform will not refuse the file for a reason we can see */
  ready: boolean
  pick: (file: File) => void
  choose: (index: number | null) => void
  clear: () => void
}

export function useCsvSource(): CsvSource {
  const [file, setFile] = useState<File | null>(null)
  const [head, setHead] = useState<CsvHead | null>(null)
  const [reading, setReading] = useState(false)
  const [readFailed, setReadFailed] = useState(false)
  const [chosen, setChosen] = useState<number | null>(null)
  const controller = useRef<AbortController | null>(null)

  useEffect(() => () => controller.current?.abort(), [])

  const pick = useCallback((next: File) => {
    controller.current?.abort()
    const c = new AbortController()
    controller.current = c
    setFile(next)
    setHead(null)
    setChosen(null)
    setReadFailed(false)
    setReading(true)
    readCsvHead(next, { signal: c.signal }).then(
      (h) => {
        if (c.signal.aborted) return
        setHead(h)
        setChosen(suggestEmailColumn(h.header))
        setReading(false)
      },
      (err: unknown) => {
        if (c.signal.aborted || isAbort(err)) return
        setReadFailed(true)
        setReading(false)
      },
    )
  }, [])

  const clear = useCallback(() => {
    controller.current?.abort()
    controller.current = null
    setFile(null)
    setHead(null)
    setChosen(null)
    setReading(false)
    setReadFailed(false)
  }, [])

  const problems = useMemo(() => (head === null || file === null ? [] : csvProblems(head, chosen, file.size)), [head, file, chosen])
  const ready = head !== null && chosen !== null && problems.length === 0

  return { file, head, reading, readFailed, chosen, problems, ready, pick, choose: setChosen, clear }
}
