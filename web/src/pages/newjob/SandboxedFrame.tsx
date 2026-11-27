// THE one place a user's own HTML is shown. It goes ONLY into an iframe, through `srcdoc`, with the strictest sandbox there is:
//
//   sandbox=""            the empty attribute: no scripts, no same origin, no forms, no popups, no top navigation, nothing allowed
//   srcdoc                the HTML itself; the frame loads no address, so nothing is fetched from the app's origin to build it
//   referrerpolicy        a request the HTML makes (an image) tells the other side nothing about where it was seen
//   title                 what the frame is, for a screen reader
//
// The app never puts a user's HTML into its own page (no innerHTML of it, no embedded object, no scripted write): a test reads this folder's sources for each.
// The page's own CSP forbids inline script as well, so a script in the HTML would run under neither rule.
import { cx } from '../../ui/index.ts'

export interface SandboxedFrameProps {
  html: string
  /** what the frame shows ("HTML of sample 1"): its accessible name */
  title: string
  className?: string
}

export function SandboxedFrame({ html, title, className }: SandboxedFrameProps) {
  return (
    <iframe
      data-testid="preview-frame"
      sandbox=""
      srcDoc={html}
      referrerPolicy="no-referrer"
      title={title}
      // An email is read on white: the frame is a light canvas in either theme (the system "Canvas" colour of a light scheme, not one of ours).
      style={{ colorScheme: 'light', backgroundColor: 'Canvas' }}
      className={cx('block w-full rounded-md border border-border', className ?? 'h-80')}
    />
  )
}
