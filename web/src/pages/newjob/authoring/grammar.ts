// The examples of the help panel. They are SYNTAX, not sentences: what a user types, shown in a code font, exactly as the platform reads it (Docs/templates.md).
// Every sentence around them is in the catalogue (copy/newjob.ts); braces and bars cannot be in a catalogue sentence, so the examples live here.
import type { CopyKey } from '../../../copy/index.ts'

export interface GrammarExample {
  write: string
  /** what it gives, in words */
  gives: CopyKey
}

export const GRAMMAR_EXAMPLES: readonly GrammarExample[] = [
  { write: 'Hi {{name}}', gives: 'newjob.help.exField' },
  { write: 'Hi {{name|there}}', gives: 'newjob.help.exFallback' },
  { write: '{Hello|Hi|Hey}', gives: 'newjob.help.exChoice' },
  { write: '{Hi|Hello} {{name|there}}', gives: 'newjob.help.exBoth' },
  { write: '\\{  \\}  \\|  \\\\', gives: 'newjob.help.exEscape' },
  { write: '<style>p \\{ color: red \\}</style>', gives: 'newjob.help.exStyle' },
  { write: '<a href="{{unsubscribe_url}}">Unsubscribe</a>', gives: 'newjob.help.exUnsubscribe' },
]
