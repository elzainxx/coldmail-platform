import { t } from '../copy/index.ts'
import { Link } from '../router.tsx'
import { buttonClass, PageHeader } from '../ui/index.ts'

export function NotFoundPage() {
  return (
    <section>
      <PageHeader title={t('common.notFound.title')} description={t('common.notFound.body')} />
      <Link to="/" className={buttonClass('primary')}>
        {t('common.notFound.home')}
      </Link>
    </section>
  )
}
