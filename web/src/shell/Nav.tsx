import { t } from '../copy/index.ts'
import { Link, useIsActive } from '../router.tsx'
import { cx } from '../ui/index.ts'
import { NAV_ITEMS, type NavItem } from './navItems.ts'

function NavLink({ item, onNavigate }: { item: NavItem; onNavigate?: () => void }) {
  const active = useIsActive(item.to, item.end === true)
  return (
    <Link
      to={item.to}
      aria-current={active ? 'page' : undefined}
      onClick={onNavigate}
      className={cx(
        'flex h-8 items-center rounded-md px-3 text-base no-underline',
        active ? 'bg-hover font-medium text-fg' : 'text-fg-muted hover:bg-hover hover:text-fg',
      )}
    >
      {t(item.label)}
    </Link>
  )
}

export interface NavProps {
  id?: string
  className?: string
  /** called when a link is followed (closes the menu on a small screen) */
  onNavigate?: () => void
}

/** The main navigation: the seven areas of the app. */
export function Nav({ id, className, onNavigate }: NavProps) {
  return (
    <nav id={id} aria-label={t('common.nav.label')} className={className}>
      <ul className="flex flex-col gap-0.5">
        {NAV_ITEMS.map((item) => (
          <li key={item.id}>
            <NavLink item={item} onNavigate={onNavigate} />
          </li>
        ))}
      </ul>
    </nav>
  )
}
