import { describe, expect, it } from 'vitest'
import { CsvParser } from './parser.ts'
import { readCsvHead, SAMPLE_ROWS, type BlobLike, type CsvPieces } from './reader.ts'

const enc = new TextEncoder()

/** The text in pieces of `size` characters (a piece may end anywhere, a quoted cell and a CRLF included). */
async function* textPieces(text: string, size: number): AsyncGenerator<string> {
  for (let i = 0; i < text.length; i += size) yield text.slice(i, i + size)
}

/** The bytes of the text in pieces of `size` bytes: a multi-byte character is cut by a piece boundary. */
async function* bytePieces(text: string, size: number): AsyncGenerator<Uint8Array> {
  const bytes = enc.encode(text)
  for (let i = 0; i < bytes.length; i += size) yield bytes.slice(i, i + size)
}

const read = (text: string, size = 1 << 20) => readCsvHead(textPieces(text, size))

describe('the CSV reader: what a cell is', () => {
  it('reads commas, doubled quotes and line breaks inside a quoted cell as part of the cell, and a row with a line break as one row', async () => {
    const text = 'email,note\r\na@x.test,"one, two"\r\nb@x.test,"she said ""hi"""\r\nc@x.test,"line one\r\nline two"\r\n'
    const head = await read(text)
    expect(head.header).toEqual(['email', 'note'])
    expect(head.rows).toEqual([
      ['a@x.test', 'one, two'],
      ['b@x.test', 'she said "hi"'],
      ['c@x.test', 'line one\nline two'], // a CRLF inside a quoted cell reads as one line feed, as the platform reads it
    ])
    expect(head.totalRows).toBe(3)
    expect(head.raggedRows).toBe(0)
    expect(head.unclosedQuote).toBe(false)
  })

  it('control: a reader that splits on line breaks and commas gets the same text wrong, so the input really is tricky', () => {
    const text = 'email,note\r\na@x.test,"one, two"\r\nc@x.test,"line one\r\nline two"\r\n'
    const naive = text.split(/\r?\n/).filter((l) => l !== '').map((l) => l.split(','))
    expect(naive.length).toBe(4) // the real count is 2 rows and a header: 3 records
    expect(naive[1]).toHaveLength(3) // "one, two" was cut into two cells
  })

  it('drops a byte order mark at the start (bytes or text) and keeps one that is not at the start', async () => {
    const withBom = '\uFEFFemail,name\na@x.test,Ann\n'
    expect((await readCsvHead(bytePieces(withBom, 1000))).header).toEqual(['email', 'name'])
    expect((await readCsvHead(bytePieces(withBom, 1))).header).toEqual(['email', 'name']) // the BOM's three bytes in three pieces
    expect((await readCsvHead(textPieces(withBom, 4))).header).toEqual(['email', 'name'])
    expect((await read('email,name\n\uFEFFa@x.test,Ann\n')).rows[0]?.[0]).toBe('\uFEFFa@x.test')
  })

  it('control: the input does carry a BOM that a decoder keeps when told to', () => {
    const kept = new TextDecoder('utf-8', { ignoreBOM: true }).decode(enc.encode('\uFEFFemail'))
    expect(kept.charCodeAt(0)).toBe(0xfeff)
  })

  it('reads CRLF, LF and a lone CR the way the platform does: CRLF and LF end a record, a CR alone is data, and one that ends the file is dropped', async () => {
    const crlf = await read('a,b\r\n1,2\r\n3,4\r\n')
    const lf = await read('a,b\n1,2\n3,4\n')
    expect(crlf.rows).toEqual(lf.rows)
    expect(crlf.totalRows).toBe(2)
    expect((await read('a,b\n1,x\ry\n')).rows).toEqual([['1', 'x\ry']])
    expect((await read('a,b\n1,2\r')).rows).toEqual([['1', '2']])
  })

  it('does not count a blank line, a final line break, or a last row with no line break after it wrongly', async () => {
    expect((await read('a,b\n1,2\n')).totalRows).toBe(1)
    expect((await read('a,b\n1,2')).totalRows).toBe(1)
    expect((await read('a,b\n1,2\n\n\n')).totalRows).toBe(1)
    expect((await read('a,b\n\n1,2\n\n3,4\n')).totalRows).toBe(2)
    expect((await read('')).totalRows).toBe(0)
    expect((await read('')).header).toEqual([])
    expect((await read('a,b\n')).totalRows).toBe(0)
    // a last cell that is empty is still a cell
    expect((await read('a,b\n1,\n')).rows).toEqual([['1', '']])
    expect((await read('a,b\n1,')).rows).toEqual([['1', '']])
  })

  it('control: a quoted empty cell on its own is a row, a blank line is not', async () => {
    expect((await read('a\n""\n')).totalRows).toBe(1)
    expect((await read('a\n\n')).totalRows).toBe(0)
  })

  it('says on which line a row starts, a quoted line break counted', async () => {
    const parser = new CsvParser({ keepRecords: 10, maxCellChars: 100 })
    parser.push('h1,h2\n"a\nb",c\nd,e\n')
    const s = parser.finish()
    expect(s.kept.map((r) => r.line)).toEqual([1, 2, 4])
  })
})

describe('the CSV reader: where a piece ends does not matter', () => {
  const tricky = 'email,note,city\r\n"a@x.test","he said ""hi"", twice","Zürich"\r\n\r\nb@x.test,"two\r\nlines",東京\r\n😀@x.test,,"x"\r\nlast@x.test,end,'

  it('gives the same answer for every way of cutting the text in two, and in single characters (and the same for bytes, cutting inside a character)', async () => {
    const whole = await read(tricky)
    expect(whole.totalRows).toBe(4)
    expect(whole.rows[1]).toEqual(['b@x.test', 'two\nlines', '東京'])
    for (let cut = 0; cut <= tricky.length; cut++) {
      const pieces: CsvPieces = (async function* () {
        yield tricky.slice(0, cut)
        yield tricky.slice(cut)
      })()
      const got = await readCsvHead(pieces)
      expect(got.rows, `cut at ${cut}`).toEqual(whole.rows)
      expect(got.totalRows, `cut at ${cut}`).toBe(whole.totalRows)
    }
    expect((await readCsvHead(textPieces(tricky, 1))).rows).toEqual(whole.rows)
    for (const size of [1, 2, 3, 5, 7]) {
      const got = await readCsvHead(bytePieces(tricky, size))
      expect(got.rows, `${size} bytes at a time`).toEqual(whole.rows)
      expect(got.header).toEqual(whole.header)
    }
  })

  it('control: reading each piece on its own (a parser that forgets where it was) does get some cut wrong', async () => {
    const text = 'a,b\n"x,y",z\n'
    let wrong = 0
    for (let cut = 1; cut < text.length; cut++) {
      const first = new CsvParser({ keepRecords: 5, maxCellChars: 100 })
      first.push(text.slice(0, cut))
      const second = new CsvParser({ keepRecords: 5, maxCellChars: 100 })
      second.push(text.slice(cut))
      if (first.finish().records + second.finish().records !== 2) wrong++
    }
    expect(wrong).toBeGreaterThan(0)
  })
})

describe('the CSV reader: what is wrong with a file', () => {
  it('counts the rows that do not have the header\'s number of cells and says where the first is; a clean file reports none', async () => {
    const bad = await read('a,b,c\n1,2,3\n4,5\n6,7,8,9\n')
    expect(bad.raggedRows).toBe(2)
    expect(bad.firstRaggedLine).toBe(3)
    const clean = await read('a,b,c\n1,2,3\n4,5,6\n')
    expect(clean.raggedRows).toBe(0)
    expect(clean.firstRaggedLine).toBeNull()
  })

  it('reports a quote that is opened and never closed; a closed one is not reported', async () => {
    expect((await read('a,b\n1,"2\n3,4\n')).unclosedQuote).toBe(true)
    expect((await read('a,b\n1,"2"\n3,4\n')).unclosedQuote).toBe(false)
  })

  it('reads a quote where the rules do not expect one as the plain character, and leaves the refusal to the platform', async () => {
    const head = await read('a,b\nx"y,"p"q\n')
    expect(head.rows).toEqual([['x"y', 'pq']])
    expect(head.unclosedQuote).toBe(false)
  })
})

describe('the CSV reader: it keeps the header and the first rows, and counts the rest', () => {
  it('keeps at most 20 rows and counts all of them', async () => {
    const lines = ['email']
    for (let i = 0; i < 25; i++) lines.push(`u${i}@x.test`)
    const head = await read(lines.join('\n'))
    expect(head.rows).toHaveLength(SAMPLE_ROWS)
    expect(head.rows[19]).toEqual(['u19@x.test'])
    expect(head.totalRows).toBe(25)
    const few = await read('email\na@x.test\n')
    expect(few.rows).toHaveLength(1) // control: a file with fewer rows than the sample is not padded
  })

  /** 100,000 rows made one piece at a time: nothing but the piece being read exists at any moment. */
  function syntheticStream(rows: number, pieceChars: number): { pieces: CsvPieces; total: () => number } {
    let produced = 0
    return {
      total: () => produced,
      pieces: (async function* () {
        let buffer = 'email,first_name,company,notes\n'
        for (let i = 0; i < rows; i++) {
          buffer += `user${i}@example.test,Name ${i},"Company, ${i % 97}",a note for row ${i} that is a little longer\n`
          if (buffer.length >= pieceChars) {
            produced += buffer.length
            yield buffer
            buffer = ''
          }
        }
        produced += buffer.length
        yield buffer
      })(),
    }
  }

  it('reads a 100,000-row stream, counts every row, keeps 20 and never holds more than a few thousand characters (it did not buffer the stream)', async () => {
    const { pieces, total } = syntheticStream(100_000, 32 * 1024)
    const head = await readCsvHead(pieces)
    expect(head.totalRows).toBe(100_000)
    expect(head.rows).toHaveLength(SAMPLE_ROWS)
    expect(head.rows[0]).toEqual(['user0@example.test', 'Name 0', 'Company, 0', 'a note for row 0 that is a little longer'])
    expect(total()).toBeGreaterThan(8_000_000) // a file of some 9 MB went by...
    expect(head.pieces).toBeGreaterThan(200) // ...in more than 200 pieces...
    expect(head.peakHeldChars).toBeLessThan(5_000) // ...and at no moment did the reader hold more than the sample and one record
  })

  it('control: a parser that keeps every record, given the same stream, holds the whole file, and the measure shows it', async () => {
    const { pieces, total } = syntheticStream(5_000, 32 * 1024)
    const parser = new CsvParser({ keepRecords: Number.MAX_SAFE_INTEGER, maxCellChars: 1_000_000 })
    for await (const piece of pieces) parser.push(piece as string)
    const summary = parser.finish()
    expect(summary.peakHeldChars).toBeGreaterThan(total() * 0.5) // it held the file
    expect(summary.peakHeldChars).toBeGreaterThan(5_000) // so the bound of the test above can fail
  })

  it('does not let one runaway quoted cell make it hold the rest of the file', async () => {
    const runaway = (async function* () {
      yield 'a,b\n1,"'
      for (let i = 0; i < 160; i++) yield 'x'.repeat(32 * 1024) // 5 MB inside a quote that never closes
    })()
    const head = await readCsvHead(runaway)
    expect(head.unclosedQuote).toBe(true)
    expect(head.truncatedCells).toBe(1)
    expect(head.peakHeldChars).toBeLessThan(5_000)
    // control: the cap is what holds it
    const uncapped = new CsvParser({ keepRecords: 21, maxCellChars: 10_000_000 })
    uncapped.push('a,b\n1,"')
    for (let i = 0; i < 40; i++) uncapped.push('x'.repeat(32 * 1024))
    expect(uncapped.finish().peakHeldChars).toBeGreaterThan(1_000_000)
  })
})

describe('the CSV reader: a file from the picker', () => {
  const csv = 'email,name\r\n"a@x.test","Zoë ""Z"""\r\nb@x.test,Bo\r\n'

  it('reads a File (a jsdom one has no stream, so it is read in slices) cut every few bytes, a multi-byte character included', async () => {
    const file = new File([csv], 'list.csv', { type: 'text/csv' })
    for (const chunkBytes of [1, 3, 64 * 1024]) {
      const head = await readCsvHead(file, { chunkBytes })
      expect(head.header, `${chunkBytes}`).toEqual(['email', 'name'])
      expect(head.rows, `${chunkBytes}`).toEqual([
        ['a@x.test', 'Zoë "Z"'],
        ['b@x.test', 'Bo'],
      ])
      expect(head.bytes).toBe(file.size)
    }
  })

  it('reads the stream of a Blob that has one (a browser\'s) and lets it go', async () => {
    const bytes = enc.encode(csv)
    let cancelled = false
    const blob: BlobLike = {
      size: bytes.length,
      slice: () => {
        throw new Error('a Blob that can stream is not sliced')
      },
      stream: () =>
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(bytes.slice(0, 5))
            controller.enqueue(bytes.slice(5, 11))
            controller.enqueue(bytes.slice(11))
            controller.close()
          },
          cancel() {
            cancelled = true
          },
        }),
    }
    const head = await readCsvHead(blob)
    expect(head.rows).toHaveLength(2)
    expect(head.pieces).toBe(3)
    expect(cancelled).toBe(false) // the stream ended on its own: nothing was left to cancel
  })

  it('stops, and lets the stream go, when it is told to', async () => {
    const controller = new AbortController()
    let cancelled = false
    const blob: BlobLike = {
      size: 1000,
      slice: () => {
        throw new Error('not sliced')
      },
      stream: () =>
        new ReadableStream<Uint8Array>({
          pull(c) {
            c.enqueue(enc.encode('a,b\n1,2\n'))
          },
          cancel() {
            cancelled = true
          },
        }),
    }
    const reading = readCsvHead(blob, { signal: controller.signal })
    controller.abort()
    await expect(reading).rejects.toMatchObject({ name: 'AbortError' })
    expect(cancelled).toBe(true)
    // control: the same read, not aborted, ends on its own
    const finite = await readCsvHead(new File(['a\n1\n'], 'x.csv'))
    expect(finite.totalRows).toBe(1)
  })
})
