import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { t } from '../../copy/index.ts'
import { mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { SandboxedFrame } from './SandboxedFrame.tsx'
import { csvFile, sendJob } from './testing/fixtures.ts'

beforeEach(async () => {
  resetForTest()
  await signInForTest()
})
afterEach(() => {
  vi.unstubAllGlobals()
  resetForTest()
})

const SCRIPT = '<script>alert(1)</script>'
const EVIL = `<p>Hi</p>${SCRIPT}<img src="x" onerror="alert(2)"><object data="x"></object><embed src="x">`

/** What is wrong with an iframe that shows a user's HTML: the empty sandbox attribute, a srcdoc and no address are the whole rule. */
function frameViolations(frame: Element): string[] {
  const bad: string[] = []
  if (frame.tagName !== 'IFRAME') return ['not an iframe']
  if (!frame.hasAttribute('sandbox')) bad.push('no sandbox attribute')
  else if (frame.getAttribute('sandbox') !== '') bad.push(`sandbox is "${frame.getAttribute('sandbox')}", not the empty attribute`)
  if (frame.hasAttribute('src')) bad.push('has a src')
  if (!frame.hasAttribute('srcdoc')) bad.push('no srcdoc')
  if (frame.getAttribute('referrerpolicy') !== 'no-referrer') bad.push('referrerpolicy is not no-referrer')
  if ((frame.getAttribute('title') ?? '').trim() === '') bad.push('no title')
  return bad
}

/** Elements that run or embed: a script, an object, an embed. None may exist in the app's own document. */
function activeElementsIn(root: ParentNode): Element[] {
  return [...root.querySelectorAll('script, object, embed, applet')]
}

describe('the sandboxed frame', () => {
  it('has sandbox="" (the empty attribute, no token at all), a srcdoc with the HTML as it is, no src, no referrer, and a title', () => {
    const { container } = render(<SandboxedFrame html={EVIL} title="Sample" />)
    const frame = container.querySelector('iframe') as HTMLIFrameElement
    expect(frame.getAttribute('sandbox')).toBe('')
    expect(frame.hasAttribute('sandbox')).toBe(true)
    expect(frame.getAttribute('srcdoc')).toBe(EVIL)
    expect(frame.hasAttribute('src')).toBe(false)
    expect(frame.getAttribute('referrerpolicy')).toBe('no-referrer')
    expect(frame.getAttribute('title')).toBe('Sample')
    expect(frame.getAttribute('data-testid')).toBe('preview-frame')
    expect(frameViolations(frame)).toEqual([])
    // nothing else is in the app's own document: the HTML is an attribute's value
    expect(container.querySelectorAll('iframe')).toHaveLength(1)
    expect(activeElementsIn(document)).toEqual([])
    expect(container.querySelector('img')).toBeNull()
  })

  it('control: the check that reads a frame does find each way of loosening it', () => {
    const make = (attrs: Record<string, string>) => {
      const el = document.createElement('iframe')
      for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v)
      return el
    }
    const good = { sandbox: '', srcdoc: 'x', referrerpolicy: 'no-referrer', title: 'T' }
    expect(frameViolations(make(good))).toEqual([])
    expect(frameViolations(make({ ...good, sandbox: 'allow-scripts' }))[0]).toContain('allow-scripts')
    expect(frameViolations(make({ ...good, sandbox: 'allow-same-origin allow-scripts' }))[0]).toContain('not the empty attribute')
    expect(frameViolations(make({ srcdoc: 'x', referrerpolicy: 'no-referrer', title: 'T' }))).toEqual(['no sandbox attribute'])
    expect(frameViolations(make({ ...good, src: '/v1/jobs' }))).toEqual(['has a src'])
    expect(frameViolations(make({ ...good, referrerpolicy: 'origin' }))).toEqual(['referrerpolicy is not no-referrer'])
    expect(frameViolations(make({ ...good, title: ' ' }))).toEqual(['no title'])
    expect(frameViolations(document.createElement('div'))).toEqual(['not an iframe'])
  })

  it('control: the scan for scripts and embedded objects does find one in a document that has it', () => {
    const div = document.createElement('div')
    div.innerHTML = '<p>x</p><object data="x"></object><embed src="y">'
    expect(activeElementsIn(div).map((e) => e.tagName)).toEqual(['OBJECT', 'EMBED'])
    const script = document.createElement('script')
    div.appendChild(script)
    expect(activeElementsIn(div).map((e) => e.tagName)).toContain('SCRIPT')
  })
})

describe('a template with a script in it', () => {
  it('is only ever put in the srcdoc of sandboxed frames: the authoring preview shows it without a script, an object or an image in the app\'s document', async () => {
    const alert = vi.fn()
    vi.stubGlobal('alert', alert)
    mockApi({
      'GET /v1/me': { email: 'a@b.test' },
      'GET /v1/balances': { modules: { m1: { available: 1, reserved: 0 }, m2: { available: 1, reserved: 0 } }, m3: { enabled: true, until: null, state: 'active' } },
      'GET /v1/m3/accounts': { accounts: [], next_cursor: null },
      'POST /v1/templates/check': { ok: true, fields: [] },
    })
    renderApp('/new/send')
    await screen.findByRole('heading', { level: 1, name: t('newjob.send.title') })
    fireEvent.click(screen.getByRole('radio', { name: t('newjob.sender.managed') }))
    fireEvent.click(screen.getByRole('button', { name: t('newjob.step.next') }))
    await screen.findByRole('heading', { level: 2, name: /Step 2 of 4/ })
    fireEvent.change(screen.getByLabelText(t('newjob.csv.file')), { target: { files: [csvFile('email,name\na@x.test,Ann\n')] } })
    await screen.findByLabelText(t('newjob.columns.address'))
    fireEvent.click(screen.getByRole('button', { name: t('newjob.step.next') }))
    await screen.findByRole('heading', { level: 2, name: /Step 3 of 4/ })

    fireEvent.change(screen.getByLabelText(t('newjob.authoring.html')), { target: { value: EVIL } })
    fireEvent.change(screen.getByLabelText(t('newjob.authoring.subject')), { target: { value: SCRIPT } })
    fireEvent.change(screen.getByLabelText(new RegExp(t('newjob.authoring.text'))), { target: { value: SCRIPT } })
    const frame = await screen.findByTitle(t('newjob.authoring.previewFrame'), undefined, { timeout: 2000 })

    expect(frameViolations(frame)).toEqual([])
    expect(frame.getAttribute('srcdoc')).toContain(SCRIPT)
    // the same text typed in the other boxes is a value of a box, not markup
    expect((screen.getByLabelText(t('newjob.authoring.subject')) as HTMLInputElement).value).toBe(SCRIPT)
    expect(activeElementsIn(document)).toEqual([])
    expect(document.querySelector('img')).toBeNull()
    expect(alert).not.toHaveBeenCalled()
    // and the page's own text holds none of it: it is in the boxes the user typed it in (their text), and in the frame's srcdoc, nowhere else
    const page = document.body.cloneNode(true) as HTMLElement
    page.querySelectorAll('textarea, input, iframe').forEach((el) => el.remove())
    expect(page.textContent).not.toContain('alert(1)')
    expect(document.body.textContent).toContain('alert(1)') // control: the same check does see it where it was typed
  })

  it('is only ever put in the srcdoc of the sandboxed frames of the samples: a script in the HTML, the subject or the text is shown as text or as an attribute, never as an element', async () => {
    const alert = vi.fn()
    vi.stubGlobal('alert', alert)
    const hostile = [
      { ordinal: 1, subject: '<img src=x onerror=alert(3)>', html: EVIL, text: SCRIPT },
      { ordinal: 2, subject: 'Hi', html: SCRIPT, text: '' },
      { ordinal: 3, subject: 'Bye', html: '<p>fine</p>', text: 'fine' },
    ]
    mockApi({
      'GET /v1/me': { email: 'a@b.test' },
      'GET /v1/balances': { modules: { m1: { available: 1, reserved: 0 }, m2: { available: 1, reserved: 0 } }, m3: { enabled: true, until: null, state: 'active' } },
      'GET /v1/jobs/job-send': { job: sendJob({ status: 'draft', preview: null }) },
      'POST /v1/jobs/job-send/preview': { job: sendJob({}, { samples: hostile }) },
    })
    renderApp('/new/send?job=job-send')
    await screen.findByRole('heading', { name: t('newjob.preview.title') })

    const frames = screen.getAllByTestId('preview-frame')
    expect(frames).toHaveLength(3)
    for (const frame of frames) expect(frameViolations(frame), frame.getAttribute('title') ?? '').toEqual([])
    expect(frames.map((f) => f.getAttribute('srcdoc'))).toEqual(hostile.map((s) => s.html))

    expect(activeElementsIn(document)).toEqual([])
    expect(document.querySelector('img')).toBeNull() // the subject's tag is text
    expect(within(screen.getByTestId('sample-1')).getByText('<img src=x onerror=alert(3)>')).toBeInTheDocument()
    expect(within(screen.getByTestId('sample-1')).getByRole('region', { name: t('newjob.samples.textOf', { n: 1 }) })).toHaveTextContent(SCRIPT)
    expect(alert).not.toHaveBeenCalled()
    // and no frame anywhere else on the page shows user HTML without the sandbox: every iframe of the document is one of these
    expect([...document.querySelectorAll('iframe')].every((f) => frameViolations(f).length === 0)).toBe(true)
  })

  it('changes nothing in the sandbox when the app re-renders the frame with other HTML', () => {
    const { container, rerender } = render(<SandboxedFrame html="<p>one</p>" title="T" />)
    rerender(<SandboxedFrame html={EVIL} title="T" />)
    const frame = container.querySelector('iframe') as HTMLIFrameElement
    expect(frame.getAttribute('sandbox')).toBe('')
    expect(frame.getAttribute('srcdoc')).toBe(EVIL)
    expect(activeElementsIn(document)).toEqual([])
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// The sources: the app of this area never puts user HTML anywhere but a srcdoc. The check reads the files, so a later edit that does breaks it.
// ---------------------------------------------------------------------------------------------------------------------------------------------------

const sources = import.meta.glob(['/src/pages/newjob/**/*.{ts,tsx}', '!/src/pages/newjob/**/*.test.{ts,tsx}', '!/src/pages/newjob/testing/**'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

const FORBIDDEN: Array<[string, RegExp]> = [
  ['dangerouslySetInnerHTML', /dangerouslySetInnerHTML/],
  ['innerHTML or outerHTML', /\.(innerHTML|outerHTML)\b/],
  ['insertAdjacentHTML, document.write, createContextualFragment or DOMParser', /insertAdjacentHTML|document\s*\.\s*write|createContextualFragment|DOMParser/],
  ['an object or embed element', /<\s*(object|embed|applet)\b/i],
  ['createElement of a script, an object, an embed or an iframe', /createElement\(\s*['"`](script|object|embed|applet|iframe)/i],
  ['a sandbox token', /allow-(scripts|same-origin|forms|popups|top-navigation|modals|downloads|pointer-lock|presentation|orientation-lock)/],
  ['eval or new Function', /\beval\s*\(|new\s+Function\s*\(/],
  ['a script element in JSX', /<\s*script\b/i],
]

function withoutComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1')
}

/** What a source does that puts a user's HTML into the app's own document, or loosens the frame. */
function htmlViolations(source: string): string[] {
  const code = withoutComments(source)
  const bad = FORBIDDEN.filter(([, re]) => re.test(code)).map(([what]) => what)
  // the one frame: an <iframe> written out has the empty sandbox attribute
  for (const m of code.matchAll(/<iframe\b/g)) {
    const rest = code.slice(m.index)
    const end = rest.search(/\/>|<\/iframe>/)
    if (!/\bsandbox=""/.test(end === -1 ? rest : rest.slice(0, end))) bad.push('an iframe without sandbox=""')
  }
  return bad
}

describe('the sources of this area', () => {
  it('never put a user\'s HTML anywhere but the srcdoc of the one sandboxed frame', () => {
    const files = Object.keys(sources)
    expect(files.length, 'a scan that read nothing proves nothing').toBeGreaterThanOrEqual(25)
    const found = Object.entries(sources).flatMap(([file, source]) => htmlViolations(source).map((v) => `${file}: ${v}`))
    expect(found).toEqual([])
  })

  it('has exactly one iframe, in SandboxedFrame.tsx, and it is the one the tests above read', () => {
    const withFrame = Object.entries(sources)
      .filter(([, src]) => /<iframe\b/.test(withoutComments(src)))
      .map(([file]) => file)
    expect(withFrame).toEqual(['/src/pages/newjob/SandboxedFrame.tsx'])
    const code = withoutComments(sources['/src/pages/newjob/SandboxedFrame.tsx'] ?? '')
    expect(code).toMatch(/sandbox=""/)
    expect(code).toMatch(/srcDoc=\{html\}/)
    expect(code).toMatch(/referrerPolicy="no-referrer"/)
    expect(code).not.toMatch(/\bsrc=/)
  })

  it('control: the scan does find each way of putting user HTML in the page', () => {
    const cases: Array<[string, string]> = [
      ['dangerouslySetInnerHTML', 'const a = <div dangerouslySetInnerHTML={{ __html: html }} />'],
      ['innerHTML or outerHTML', 'el.innerHTML = html'],
      ['insertAdjacentHTML, document.write, createContextualFragment or DOMParser', 'document.write(html)'],
      ['an object or embed element', 'const a = <object data={url} />'],
      ['an object or embed element', 'const a = <embed src={url} />'],
      ['createElement of a script, an object, an embed or an iframe', "document.createElement('script')"],
      ['a sandbox token', '<iframe sandbox="allow-scripts" srcDoc={html} />'],
      ['eval or new Function', 'eval(code)'],
      ['an iframe without sandbox=""', '<iframe srcDoc={html} title="x" />'],
    ]
    for (const [what, source] of cases) expect(htmlViolations(source), source).toContain(what)
    expect(htmlViolations('<iframe sandbox="allow-same-origin" srcDoc={html} />')).toContain('an iframe without sandbox=""')
  })

  it('control: the scan reads code, not comments, and passes the real frame', () => {
    expect(htmlViolations('// never dangerouslySetInnerHTML, never an <object>\nconst a = 1')).toEqual([])
    expect(htmlViolations('/* innerHTML is not used */ const a = 1')).toEqual([])
    expect(htmlViolations('<iframe sandbox="" srcDoc={html} title={t} referrerPolicy="no-referrer" />')).toEqual([])
    expect(htmlViolations(sources['/src/pages/newjob/SandboxedFrame.tsx'] ?? '')).toEqual([])
  })
})
