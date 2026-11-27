import { describe, expect, it } from 'vitest'
import { csvProblems, isFieldName, mergeColumns, suggestEmailColumn, MAX_ROWS } from './columns.ts'
import type { CsvHead } from './reader.ts'

function head(header: string[], over: Partial<CsvHead> = {}): CsvHead {
  return {
    header,
    rows: [['a@x.test']],
    totalRows: 1,
    raggedRows: 0,
    firstRaggedLine: null,
    unclosedQuote: false,
    truncatedCells: 0,
    bytes: 100,
    pieces: 1,
    peakHeldChars: 10,
    ...over,
  }
}

describe('the column picker: which column is the address', () => {
  it('pre-selects the column named email, in any letter case and with spaces around it', () => {
    expect(suggestEmailColumn(['name', 'email', 'city'])).toBe(1)
    expect(suggestEmailColumn(['Name', ' EMAIL ', 'city'])).toBe(1)
  })

  it('prefers the column named email to one that only contains mail, wherever it is', () => {
    expect(suggestEmailColumn(['mailing_list', 'name', 'email'])).toBe(2)
  })

  it('pre-selects the first column whose name contains mail when none is named email', () => {
    expect(suggestEmailColumn(['name', 'E-mail address', 'backup mail'])).toBe(1)
    expect(suggestEmailColumn(['name', 'Contact_Email'])).toBe(1)
  })

  it('pre-selects nothing when no name says mail (the user must choose), and nothing for an empty header', () => {
    expect(suggestEmailColumn(['name', 'city', 'company'])).toBeNull()
    expect(suggestEmailColumn([])).toBeNull()
  })
})

describe('the column picker: the merge fields the other columns become', () => {
  it('lists every column but the address, in order, as a field a message can name', () => {
    expect(mergeColumns(['email', 'first_name', 'Company2', '_x'], 0)).toEqual([
      { index: 1, name: 'first_name', usable: true },
      { index: 2, name: 'Company2', usable: true },
      { index: 3, name: '_x', usable: true },
    ])
  })

  it('moves with the choice: choosing another column as the address makes the old one a merge field', () => {
    expect(mergeColumns(['a', 'b', 'c'], 1).map((c) => c.name)).toEqual(['a', 'c'])
    expect(mergeColumns(['a', 'b', 'c'], 2).map((c) => c.name)).toEqual(['a', 'b'])
    expect(mergeColumns(['a', 'b'], null).map((c) => c.name)).toEqual(['a', 'b']) // control: nothing chosen, nothing taken out
  })

  it('says which names a message cannot use: a space, a dash, a leading digit, more than 64 characters', () => {
    const cols = mergeColumns(['email', 'First Name', 'e-mail 2', '2nd', 'ok_name', 'x'.repeat(65), 'x'.repeat(64)], 0)
    expect(cols.map((c) => [c.name.slice(0, 10), c.usable])).toEqual([
      ['First Name', false],
      ['e-mail 2', false],
      ['2nd', false],
      ['ok_name', true],
      ['xxxxxxxxxx', false],
      ['xxxxxxxxxx', true],
    ])
    expect(isFieldName('first_name')).toBe(true)
    expect(isFieldName('')).toBe(false)
  })

  it('trims the name it will be known by', () => {
    expect(mergeColumns(['email', '  city '], 0)[0]).toEqual({ index: 1, name: 'city', usable: true })
  })
})

describe('the column picker: what the platform will refuse in the file', () => {
  it('finds nothing wrong with a clean file, and a missing choice is not a problem of the file', () => {
    expect(csvProblems(head(['email', 'name']), 0, 100)).toEqual([])
    expect(csvProblems(head(['email', 'name']), null, 100)).toEqual([])
  })

  it('refuses an empty file, a file with a header and no rows, a file over 64 MB and one over 100,000 rows', () => {
    expect(csvProblems(head([], { totalRows: 0 }), null, 0)).toEqual([{ kind: 'empty' }])
    expect(csvProblems(head(['email'], { totalRows: 0 }), 0, 10)).toEqual([{ kind: 'noRows' }])
    expect(csvProblems(head(['email']), 0, 64 * 1024 * 1024 + 1)).toEqual([{ kind: 'tooLarge' }])
    expect(csvProblems(head(['email'], { totalRows: MAX_ROWS + 1 }), 0, 10)).toEqual([{ kind: 'tooManyRows', count: MAX_ROWS + 1 }])
    expect(csvProblems(head(['email'], { totalRows: MAX_ROWS }), 0, 64 * 1024 * 1024)).toEqual([]) // control: the limits themselves are allowed
  })

  it('refuses a column with no name and a name that is used twice (once each)', () => {
    expect(csvProblems(head(['email', 'name', '']), 0, 10)).toEqual([{ kind: 'unnamedColumn', position: 3 }])
    expect(csvProblems(head(['email', ' ', 'name']), 0, 10)).toEqual([{ kind: 'unnamedColumn', position: 2 }])
    expect(csvProblems(head(['email', 'name', 'name', 'name']), 0, 10)).toEqual([{ kind: 'repeatedColumn', name: 'name' }])
    expect(csvProblems(head(['email', 'Name', 'name']), 0, 10)).toEqual([]) // control: field names are case sensitive, these are two
  })

  it('refuses rows with another number of columns, and a quote that is never closed', () => {
    expect(csvProblems(head(['email'], { raggedRows: 2, firstRaggedLine: 7 }), 0, 10)).toEqual([{ kind: 'raggedRows', count: 2, line: 7 }])
    expect(csvProblems(head(['email'], { unclosedQuote: true }), 0, 10)).toEqual([{ kind: 'unclosedQuote' }])
  })

  it('refuses an address column that the file has twice, in any letter case', () => {
    expect(csvProblems(head(['mail', 'Mail', 'x']), 0, 10)).toEqual([{ kind: 'addressTwice', name: 'mail' }])
    expect(csvProblems(head(['mail', 'x']), 0, 10)).toEqual([])
  })

  it('refuses a column called email or unsubscribe_url beside an address column with another name, and allows them when the address column is email', () => {
    expect(csvProblems(head(['work mail', 'Email', 'x']), 0, 10)).toEqual([{ kind: 'reservedColumn', name: 'Email' }])
    expect(csvProblems(head(['work mail', 'Unsubscribe_URL']), 0, 10)).toEqual([{ kind: 'reservedColumn', name: 'Unsubscribe_URL' }])
    // control: with the address in the column called email there is nothing reserved to clash with, and the other column is just a field
    expect(csvProblems(head(['email', 'work mail']), 0, 10)).toEqual([])
    // and choosing the column that IS the reserved name clears it
    expect(csvProblems(head(['work mail', 'Email']), 1, 10)).toEqual([])
  })
})
