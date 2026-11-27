// Copy of the 'suppressions' area: every string a user reads on these screens, keyed 'suppressions.<name>'.
// Owned by the screen agent of this area; keep every key under the 'suppressions.' prefix (the type enforces it).
// Rules (copy.test.ts holds them): customer wording, no internal word, no snake_case token, no API code.
// Dynamic words from the API (a status, a reason) are written once in common.ts or errors.ts; do not copy them here.
//
// Why an address was refused (the closed list of the API) is `common.malformed.<reason>` (words.ts malformedReasonText): an unknown reason reads "not valid", never the word.
import type { AreaCopy } from './types.ts'

export const suppressions = {
  // ---- the page ----
  'suppressions.title': 'Suppression list',
  'suppressions.intro':
    'Addresses on this list never receive your messages: every send job leaves them out. People who unsubscribe are added automatically. You can also add an address yourself, or import a list.',
  'suppressions.loadError': 'We could not load the list.',

  // ---- search and the table ----
  'suppressions.search.label': 'Search the list',
  'suppressions.search.hint': 'Type part of an address.',
  'suppressions.table.caption': 'Suppressed addresses',
  'suppressions.col.address': 'Address',
  'suppressions.col.source': 'How it got here',
  'suppressions.col.when': 'When',
  'suppressions.col.actions': 'Actions',
  'suppressions.source.unsubscribe': 'They unsubscribed',
  'suppressions.source.manual': 'You added it',
  'suppressions.source.import': 'Imported from a file',
  'suppressions.source.other': 'Added',
  'suppressions.empty.title': 'No suppressed addresses yet',
  'suppressions.empty.body':
    'This list keeps messages from reaching people who should not get them. Addresses that unsubscribe from your messages appear here by themselves. You can also add an address or import a list, and no job will send to it.',
  'suppressions.empty.search': 'No address on the list contains "{query}".',

  // ---- adding one address ----
  'suppressions.add.title': 'Add an address',
  'suppressions.add.label': 'Email address',
  'suppressions.add.hint': 'No job of yours will send to it.',
  'suppressions.add.submit': 'Add to the list',
  'suppressions.add.working': 'Adding',
  'suppressions.add.empty': 'Enter the email address to add.',
  'suppressions.add.done': '{email} was added to the list.',
  'suppressions.add.already': '{email} was already on the list.',

  // ---- removing one ----
  'suppressions.remove.button': 'Remove',
  'suppressions.remove.buttonFor': 'Remove {email} from the list',
  'suppressions.remove.title': 'Remove {email} from the list?',
  'suppressions.remove.body':
    'You will be able to mail this address again: your next job will no longer leave it out. A job that has already started is not changed. We keep a record that you removed it.',
  'suppressions.remove.bodyUnsubscribed':
    'This person unsubscribed from your messages themselves. If you remove them, your next job can send to them again, and you are responsible for mailing someone who asked you to stop. Only remove them if they have asked to hear from you again. We keep a record that you removed them.',
  'suppressions.remove.confirm': 'Remove from the list',
  'suppressions.remove.confirmUnsubscribed': 'Remove anyway and allow mailing',
  'suppressions.remove.done': '{email} was removed from the list.',
  'suppressions.remove.gone': '{email} was no longer on the list.',

  // ---- importing a file ----
  'suppressions.import.title': 'Import a file',
  'suppressions.import.hint': 'Choose a CSV file whose first line names the columns, one of them email. Other columns are ignored.',
  'suppressions.import.label': 'CSV file',
  'suppressions.import.submit': 'Import the file',
  'suppressions.import.working': 'Importing',
  'suppressions.import.nothing': 'Choose a file first.',
  'suppressions.import.badFile': 'We could not read that file. Its first line must name the columns, and exactly one of them must be email.',
  'suppressions.import.summary': 'Added: {added}. Already on the list: {already}. Not valid: {invalid}.',
  'suppressions.import.sampleCaption': 'Lines that are not valid',
  'suppressions.import.sampleLine': 'Line',
  'suppressions.import.sampleWhy': 'Why',
  'suppressions.import.sampleNote': 'Showing the first {shown} of {invalid} lines that are not valid. The rest of the file was imported.',
  'suppressions.import.sampleAll': 'Every line that is not valid is shown. The rest of the file was imported.',
} as const satisfies AreaCopy<'suppressions'>
