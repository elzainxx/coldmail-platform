import { Component, type ErrorInfo, type ReactNode } from 'react'
import { t } from '../copy/index.ts'
import { buttonClass, PageHeader } from '../ui/index.ts'

interface Props {
  children: ReactNode
  /** when this changes the boundary lets the page try again (the router passes the path) */
  resetKey?: string
}

interface State {
  failed: boolean
}

/** The last line of defence: a page that throws shows a calm message instead of a blank screen. Nothing of the error is shown to the user. */
export class ErrorBoundary extends Component<Props, State> {
  override state: State = { failed: false }

  static getDerivedStateFromError(): State {
    return { failed: true }
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    // For the developer's console only: the screen says nothing of it.
    console.error('A page failed to render', error, info.componentStack)
  }

  override componentDidUpdate(prev: Props): void {
    if (this.state.failed && prev.resetKey !== this.props.resetKey) this.setState({ failed: false })
  }

  override render(): ReactNode {
    if (!this.state.failed) return this.props.children
    return (
      <section>
        <PageHeader title={t('common.boundary.title')} description={t('common.boundary.body')} />
        <div className="flex items-center gap-2">
          <button type="button" className={buttonClass('primary')} onClick={() => window.location.reload()}>
            {t('common.boundary.reload')}
          </button>
          <a href="/" className={buttonClass('secondary')}>
            {t('common.boundary.home')}
          </a>
        </div>
      </section>
    )
  }
}
