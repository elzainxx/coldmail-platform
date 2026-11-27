// Reads a CSV file for the column picker WITHOUT holding it: the header and the first rows are kept, everything after them is counted and let go.
//
//   const head = await readCsvHead(file)        // file: a File or any Blob; or an async iterable of bytes or text (the tests' synthetic stream)
//   head.header, head.rows (the first 20), head.totalRows (every data row of the file)
//
// The file is read in pieces of 64 KiB (its own stream where the browser has one, else slices), decoded as UTF-8 piece by piece (a character cut
// by a piece boundary is finished by the next piece), and fed to the parser (parser.ts), which keeps only the sample. The upload does NOT go through
// this: the platform gets the File itself, streamed from disk (api/client.ts); this reads the file only to show the user what is in it.
import { CsvParser } from './parser.ts'

/** How many data rows the picker needs. */
export const SAMPLE_ROWS = 20

/** The longest cell kept (characters). A header longer than this is not a header (see columns.ts). */
export const MAX_CELL_CHARS = 2000

const CHUNK_BYTES = 64 * 1024

/** The part of Blob this needs; a jsdom Blob has no stream(), the browser's has. */
export interface BlobLike {
  size: number
  slice(start: number, end: number): { arrayBuffer(): Promise<ArrayBuffer> }
  stream?: () => ReadableStream<Uint8Array>
}

export type CsvPieces = AsyncIterable<Uint8Array | string>

export interface CsvHead {
  /** the first record, a byte order mark removed; [] for an empty file */
  header: string[]
  /** up to SAMPLE_ROWS data records */
  rows: string[][]
  /** every data record of the file (the header is not one; blank lines are not) */
  totalRows: number
  /** data records whose number of cells is not the header's */
  raggedRows: number
  firstRaggedLine: number | null
  unclosedQuote: boolean
  truncatedCells: number
  /** the size of what was read, in bytes (characters, for a source of text) */
  bytes: number
  /** pieces read */
  pieces: number
  /** the most characters the reader held at once (the cells it kept and the record it was reading): a few thousand, whatever the size of the file */
  peakHeldChars: number
}

export interface ReadOptions {
  sampleRows?: number
  signal?: AbortSignal
  /** the size of a piece when the source is a Blob that cannot stream */
  chunkBytes?: number
}

function aborted(): DOMException {
  return new DOMException('aborted', 'AbortError')
}

/** The bytes of a Blob, piece by piece: its own stream when there is one, else slices read one at a time. At most one piece is held. */
export async function* blobPieces(blob: BlobLike, chunkBytes = CHUNK_BYTES): AsyncGenerator<Uint8Array> {
  if (typeof blob.stream === 'function') {
    const reader = blob.stream().getReader()
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) return
        yield value
      }
    } finally {
      await reader.cancel().catch(() => undefined) // a read that stops early (a new file was picked) lets the stream go
    }
  } else {
    for (let offset = 0; offset < blob.size; offset += chunkBytes) {
      yield new Uint8Array(await blob.slice(offset, offset + chunkBytes).arrayBuffer())
    }
  }
}

function isBlobLike(source: BlobLike | CsvPieces): source is BlobLike {
  return typeof (source as BlobLike).slice === 'function' && typeof (source as BlobLike).size === 'number'
}

export async function readCsvHead(source: BlobLike | CsvPieces, options: ReadOptions = {}): Promise<CsvHead> {
  const sampleRows = options.sampleRows ?? SAMPLE_ROWS
  const { signal } = options
  const pieces: CsvPieces = isBlobLike(source) ? blobPieces(source, options.chunkBytes) : source
  const parser = new CsvParser({ keepRecords: sampleRows + 1, maxCellChars: MAX_CELL_CHARS })
  const decoder = new TextDecoder('utf-8') // a byte order mark at the start is dropped by the decoder
  let bytes = 0
  let count = 0
  let first = true

  // A source of bytes loses its byte order mark in the decoder; a source of text keeps it, and it is dropped here.
  const feed = (text: string, dropBom: boolean) => {
    let s = text
    if (first && s !== '') {
      first = false
      if (dropBom && s.charCodeAt(0) === 0xfeff) s = s.slice(1)
    }
    parser.push(s)
  }

  for await (const piece of pieces) {
    if (signal?.aborted) throw aborted()
    count++
    if (typeof piece === 'string') {
      bytes += piece.length
      feed(piece, true)
    } else {
      bytes += piece.byteLength
      feed(decoder.decode(piece, { stream: true }), false)
    }
  }
  if (signal?.aborted) throw aborted()
  feed(decoder.decode(), false) // the end of a character cut short at the end of the file

  const summary = parser.finish()
  const [headerRecord, ...dataRecords] = summary.kept
  return {
    header: headerRecord?.cells ?? [],
    rows: dataRecords.map((r) => r.cells),
    totalRows: Math.max(summary.records - 1, 0),
    raggedRows: summary.raggedRows,
    firstRaggedLine: summary.firstRaggedLine,
    unclosedQuote: summary.unclosedQuote,
    truncatedCells: summary.truncatedCells,
    bytes,
    pieces: count,
    peakHeldChars: summary.peakHeldChars,
  }
}
