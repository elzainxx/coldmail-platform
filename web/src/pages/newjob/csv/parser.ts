// An incremental CSV parser: RFC 4180 as the platform's own reader (Go's encoding/csv) takes it, written so that a file of any size is read with a
// memory that does not depend on the size of the file.
//
//   push(text)   feed a piece of the file (a piece may end anywhere: inside a cell, inside a quoted cell, between a CR and its LF)
//   finish()     the file ended
//
// What it keeps: the first `keepRecords` records whole (the header and the sample the column picker shows). Every later record is only COUNTED
// (its cells are walked over and thrown away), so a file of 100,000 rows costs the same memory as the sample. A cell longer than `maxCellChars`
// is cut (and counted) so that one runaway quoted cell cannot make the parser hold the rest of the file either.
//
// The reading rules, the same as the platform's:
//   - a cell is quoted with "..." and may hold commas, line breaks and "" (a doubled quote is one quote);
//   - a record ends at a line feed; a carriage return right before it is dropped (CRLF), also inside a quoted cell; a lone carriage return is data,
//     except one that is the very last character of the file, which is dropped;
//   - a line with nothing on it is not a record (the platform skips blank lines);
//   - a byte order mark at the very start is the reader's business (reader.ts strips it).
// A quote where the rules do not expect one (a quote inside an unquoted cell, text after a closing quote) is read as the plain character: the
// platform may refuse such a file, and its answer is the one the user gets. Only an unclosed quote at the end of the file is reported here,
// because the platform certainly refuses that.

export interface ParserOptions {
  /** how many records (the header counts as the first) are kept whole */
  keepRecords: number
  /** the longest cell kept, in characters; a longer one is cut */
  maxCellChars: number
}

export interface KeptRecord {
  /** the line of the file the record starts on (1-based; a quoted cell with line breaks makes a record span lines) */
  line: number
  cells: string[]
}

/** What the parser knows at the end. */
export interface ParseSummary {
  /** the first `keepRecords` records: the header first */
  kept: KeptRecord[]
  /** the records of the file, the header included; blank lines are not records */
  records: number
  /** records after the header whose number of cells is not the header's */
  raggedRows: number
  /** the line the first of them starts on */
  firstRaggedLine: number | null
  /** the file ended inside a quoted cell */
  unclosedQuote: boolean
  /** cells that were cut at `maxCellChars` (only among the kept records) */
  truncatedCells: number
  /**
   * The most characters the parser held at any moment, over the whole parse: the cells it keeps plus the record it is reading. The instrument that says the parser
   * did not buffer the file: it is the same for a file of ten rows and a file of a million.
   */
  peakHeldChars: number
}

type State = 'start' | 'unquoted' | 'quoted' | 'quote-seen'

const LF = 10
const CR = 13
const COMMA = 44
const QUOTE = 34

export class CsvParser {
  private readonly keepRecords: number
  private readonly maxCellChars: number

  private state: State = 'start'
  private pendingCR = false
  private finished = false

  private line = 1
  private recordLine = 1
  private records = 0
  private headerCells = 0
  private raggedRows = 0
  private firstRaggedLine: number | null = null
  private unclosedQuote = false
  private truncatedCells = 0
  private peak = 0
  private retained = 0

  // the record being read
  private cells: string[] = []
  private cellCount = 0
  private cell = ''
  private cellCut = false
  private pending = 0
  private readonly kept: KeptRecord[] = []

  constructor(options: ParserOptions) {
    this.keepRecords = options.keepRecords
    this.maxCellChars = options.maxCellChars
  }

  push(text: string): void {
    if (this.finished) throw new Error('CsvParser: push after finish')
    const input = this.pendingCR ? '\r' + text : text
    this.pendingCR = false
    const n = input.length
    for (let i = 0; i < n; i++) {
      const code = input.charCodeAt(i)
      if (code === CR) {
        if (i + 1 < n) {
          if (input.charCodeAt(i + 1) === LF) continue // CRLF: the CR goes, the LF is read next
        } else {
          this.pendingCR = true // the piece ends on a CR: the next piece says whether it was a CRLF
          continue
        }
      }
      this.step(code, input[i] as string)
    }
  }

  finish(): ParseSummary {
    if (!this.finished) {
      this.finished = true
      this.pendingCR = false // a carriage return that ends the file is dropped
      if (this.state === 'quoted') this.unclosedQuote = true
      // a last record with no line feed after it; after a final line feed the state is 'start' with nothing in it
      if (this.state !== 'start' || this.cellCount > 0) {
        this.endCell()
        this.endRecord()
      }
    }
    return {
      kept: this.kept,
      records: this.records,
      raggedRows: this.raggedRows,
      firstRaggedLine: this.firstRaggedLine,
      unclosedQuote: this.unclosedQuote,
      truncatedCells: this.truncatedCells,
      peakHeldChars: this.peak,
    }
  }

  private step(code: number, ch: string): void {
    if (code === LF) this.line++
    switch (this.state) {
      case 'start':
        if (code === QUOTE) {
          this.state = 'quoted'
        } else if (code === COMMA) {
          this.endCell()
        } else if (code === LF) {
          if (this.cellCount === 0) this.recordLine = this.line // a blank line is not a record
          else {
            this.endCell()
            this.endRecord()
          }
        } else {
          this.state = 'unquoted'
          this.add(ch)
        }
        return
      case 'unquoted':
        if (code === COMMA) {
          this.endCell()
        } else if (code === LF) {
          this.endCell()
          this.endRecord()
        } else {
          this.add(ch)
        }
        return
      case 'quoted':
        if (code === QUOTE) this.state = 'quote-seen'
        else this.add(ch)
        return
      case 'quote-seen':
        if (code === QUOTE) {
          this.add('"') // "" is one quote
          this.state = 'quoted'
        } else if (code === COMMA) {
          this.endCell()
        } else if (code === LF) {
          this.endCell()
          this.endRecord()
        } else {
          this.add(ch) // text after a closing quote: read as plain text, the platform may refuse it
          this.state = 'unquoted'
        }
        return
    }
  }

  /** Is the record being read one whose cells are kept? */
  private keeping(): boolean {
    return this.records < this.keepRecords
  }

  private add(ch: string): void {
    if (!this.keeping()) return // a record past the sample: its text is walked over, never stored
    if (this.cell.length >= this.maxCellChars) {
      if (!this.cellCut) {
        this.cellCut = true
        this.truncatedCells++
      }
      return
    }
    this.cell += ch
    this.pending++
    if (this.retained + this.pending > this.peak) this.peak = this.retained + this.pending
  }

  private endCell(): void {
    this.cellCount++
    if (this.keeping()) this.cells.push(this.cell)
    this.cell = ''
    this.cellCut = false
    this.state = 'start'
  }

  private endRecord(): void {
    const index = this.records
    this.records++
    if (index === 0) {
      this.headerCells = this.cellCount
    } else if (this.cellCount !== this.headerCells) {
      this.raggedRows++
      if (this.firstRaggedLine === null) this.firstRaggedLine = this.recordLine
    }
    if (index < this.keepRecords) {
      this.kept.push({ line: this.recordLine, cells: this.cells })
      this.retained += this.pending
    }
    this.cells = []
    this.cellCount = 0
    this.pending = 0
    this.recordLine = this.line // the next record starts on the line after the line feed that ended this one
  }
}
