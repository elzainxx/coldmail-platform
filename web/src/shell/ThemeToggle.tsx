import { useState } from 'react'
import { t } from '../copy/index.ts'
import { IconButton, MoonIcon, SunIcon } from '../ui/index.ts'
import { chooseTheme, currentTheme, type Theme } from './theme.ts'

/** One button that switches between light and dark. Its name says what it will do, so it reads right in either state. */
export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(currentTheme)
  const next: Theme = theme === 'dark' ? 'light' : 'dark'
  return (
    <IconButton
      label={next === 'dark' ? t('common.theme.toDark') : t('common.theme.toLight')}
      onClick={() => {
        chooseTheme(next)
        setTheme(next)
      }}
    >
      {theme === 'dark' ? <SunIcon /> : <MoonIcon />}
    </IconButton>
  )
}
