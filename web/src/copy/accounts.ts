// Copy of the 'accounts' area: every string a user reads on these screens, keyed 'accounts.<name>'.
// Owned by the screen agent of this area; keep every key under the 'accounts.' prefix (the type enforces it).
// Rules (copy.test.ts holds them): customer wording, no internal word, no snake_case token, no API code.
// Dynamic words from the API (a status, a reason) are written once in common.ts or errors.ts; do not copy them here.
//
// The refusals of the account routes (a duplicate, a host that is not allowed, a rate limit, an outcome that is not known...) have their line in errors.ts
// (`error.<code>`); the issue of one field of a refused account is `error.field.<issue>`. This file holds what only these screens say.
import type { AreaCopy } from './types.ts'

export const accounts = {
  // ---- the page ----
  'accounts.title': 'SMTP accounts',
  'accounts.intro':
    'The mailboxes you send from when a campaign goes out through your own accounts. Add the details your email provider gives you. Before an account is used we log in to check it and look at your From address.',
  'accounts.add.open': 'Add an account',
  'accounts.bulk.open': 'Add many accounts',
  'accounts.loadError': 'We could not load your accounts.',

  // ---- whether sending through your own accounts is switched on (what the balances say) ----
  'accounts.entitlement.none':
    'Sending through your own accounts is not switched on for you. You can look at and remove accounts here, but adding or testing one will not work until it is. Contact support to have it switched on.',
  'accounts.entitlement.ended':
    'Your access to sending through your own accounts ended on {date}. You can look at and remove accounts here, but adding or testing one will not work until it is renewed. Contact support to renew it.',
  'accounts.entitlement.suspended':
    'Sending through your own accounts is suspended for you. You can look at and remove accounts here, but adding or testing one will not work. Contact support.',
  'accounts.entitlement.until': 'Sending through your own accounts is switched on until {date}.',

  // ---- the list ----
  'accounts.list.caption': 'Your SMTP accounts',
  'accounts.col.address': 'Sending address',
  'accounts.col.server': 'Mail server',
  'accounts.col.status': 'Status',
  'accounts.col.fromCheck': 'Sending address check',
  'accounts.col.actions': 'Actions',
  'accounts.row.name': 'Shown as {name}',
  'accounts.row.login': 'Login: {username}',
  'accounts.row.limit': 'Up to {count} messages a day',
  'accounts.row.limitOne': 'Up to 1 message a day',
  'accounts.empty.title': 'No SMTP accounts yet',
  'accounts.empty.body':
    'Add the account of the mailbox you want to send from. A campaign that sends through your own accounts needs at least one that works.',

  // ---- the state of an account ----
  'accounts.status.active': 'Active',
  'accounts.status.paused': 'Paused',
  'accounts.status.quarantined': 'On hold',
  'accounts.status.disabled': 'Disabled',
  'accounts.pause.dailyLimit': 'It reached its daily limit and starts again tomorrow.',
  'accounts.pause.throttle': 'The mail provider asked us to slow down, so sending is paused for a while.',
  'accounts.pause.manual': 'It was paused by hand.',
  'accounts.hold.authFailure': 'The mail server refused the login. Check the password, then use Test.',
  'accounts.hold.consecutiveFailures': 'Several messages in a row could not be sent.',
  'accounts.hold.failureRate': 'Too many of its messages could not be sent.',
  'accounts.hold.manual': 'It was put on hold by hand.',

  // ---- the login check of an account ----
  'accounts.verification.verified': 'The login works.',
  'accounts.verification.unverified': 'The login has not been checked yet.',
  'accounts.verification.failed': 'The last login check failed: {why}.',
  'accounts.why.connectFailed': 'we could not connect to the mail server',
  'accounts.why.timeout': 'the mail server did not answer in time',
  'accounts.why.tlsUnavailable': 'the mail server does not offer an encrypted connection',
  'accounts.why.tlsFailed': 'the encrypted connection could not be set up',
  'accounts.why.authFailed': 'the mail server refused the username or password',
  'accounts.why.protocolError': 'the mail server answered in a way we did not expect',
  'accounts.why.connectionLost': 'the connection was lost',
  'accounts.why.hostNotAllowed': 'we cannot connect to that mail server address',
  'accounts.why.error': 'something went wrong',
  'accounts.why.unavailable': 'the check could not run',

  // ---- the check of the From address: a word and one line each ----
  'accounts.fromGate.aligned': 'Aligned',
  'accounts.fromGate.alignedWhy': 'Your From address matches the mail server, so providers are likely to accept your mail.',
  'accounts.fromGate.unverified': 'Not checked yet',
  'accounts.fromGate.unverifiedWhy': 'We have not checked this From address yet. Use Test to check it.',
  'accounts.fromGate.warned': 'Warning: not fully aligned',
  'accounts.fromGate.warnedWhy':
    'Your From address does not fully line up with this mail server, so providers may treat your mail as spam. Use Test to read the warning and confirm it.',
  'accounts.fromGate.blocked': 'Blocked',
  'accounts.fromGate.blockedWhy':
    'We will not send from this address, because providers would reject the mail. Use a From address on a domain you control, then add the account again.',
  'accounts.fromGate.indeterminate': 'Could not be determined',
  'accounts.fromGate.indeterminateWhy': 'We could not look up the records for this address just now. Try Test again in a little while.',

  // ---- the form that adds one account ----
  'accounts.add.title': 'Add an SMTP account',
  'accounts.add.intro': 'We use these details to log in to your mail server and to send your campaigns. The password is sent once and never shown again.',
  'accounts.add.host': 'Mail server',
  'accounts.add.hostHint': 'The server name your provider gives you, for example smtp.example.com.',
  'accounts.add.port': 'Port',
  'accounts.add.portHint':
    'Leave it empty to use 587. Port 465 connects encrypted from the start; any other port switches to an encrypted connection after connecting.',
  'accounts.add.username': 'Username',
  'accounts.add.password': 'Password',
  'accounts.add.passwordHint': 'The password or app password of the mailbox. It is used to log in and is never shown again.',
  'accounts.add.fromAddress': 'From address',
  'accounts.add.fromAddressHint': 'The address your recipients see.',
  'accounts.add.fromName': 'From name',
  'accounts.add.fromNameHint': 'The name your recipients see next to the address.',
  'accounts.add.dailyLimit': 'Daily limit',
  'accounts.add.dailyLimitHint': 'The most messages to send through this account in a day. Leave it empty to use ours.',
  'accounts.add.submit': 'Add the account',
  'accounts.add.submitAnyway': 'Add the account anyway',
  'accounts.add.working': 'Adding and checking the account. This can take a few seconds.',

  // ---- the warning about the From address that has to be confirmed ----
  'accounts.ack.title': 'A warning about your From address',
  'accounts.ack.body':
    'Your From address does not fully line up with this mail server, so providers may treat your mail as spam. You can still use the account if you accept that.',
  'accounts.ack.checkbox': 'I understand the warning and want to add this account anyway',
  'accounts.ack.checkboxUse': 'I understand the warning and want to use this account anyway',
  'accounts.ack.needed': 'Tick the box to confirm that you understand the warning.',

  // ---- what an add came to ----
  'accounts.added.title': 'Added {address}',
  'accounts.added.dismiss': 'Dismiss',
  'accounts.added.toast': 'The account {address} was added.',
  'accounts.added.loginFailed': 'The account was added, but the login check failed: {why}. Check the details and use Test, or remove the account and add it again.',
  'accounts.added.loginUnchecked': 'The account was added, but we could not check the login yet. Use Test to check it.',

  // ---- the Test button ----
  'accounts.test.button': 'Test',
  'accounts.test.buttonFor': 'Test {address}',
  'accounts.test.title': 'Test of {address}',
  'accounts.test.done': 'Done',
  'accounts.test.connection': 'Connection',
  'accounts.test.works': 'Works',
  'accounts.test.fails': 'Does not work',
  'accounts.test.fromCheck': 'Sending address check',
  'accounts.test.again': 'Test again and confirm',
  'accounts.test.confirmed': 'You confirmed this warning.',
  'accounts.test.working': 'Testing the account. This can take a few seconds.',
  'accounts.stage.done': 'The connection works and the mail server accepted the login.',
  'accounts.stage.connect': 'We could not connect to the mail server. Check the server name and the port.',
  'accounts.stage.host': 'We cannot connect to that mail server address. Check the server name, or use the one your email provider gives you.',
  'accounts.stage.tls': 'The mail server could not set up an encrypted connection. Check the port.',
  'accounts.stage.login': 'The mail server refused the username or password. Check them and try again.',
  'accounts.stage.protocol': 'The mail server answered in a way we did not expect. Try again later.',
  'accounts.stage.unavailable': 'We could not run the check just now. Try again in a while.',

  // ---- removing an account ----
  'accounts.remove.button': 'Remove',
  'accounts.remove.buttonFor': 'Remove {address}',
  'accounts.remove.title': 'Remove {address}?',
  'accounts.remove.body':
    'This account stops being used at once. No new message is sent through it, including by campaigns that are running now. You cannot undo this here: to use the account again you have to add it again with its password.',
  'accounts.remove.confirm': 'Remove the account',
  'accounts.remove.done': 'The account {address} was removed.',

  // ---- adding many ----
  'accounts.bulk.title': 'Add many SMTP accounts',
  'accounts.bulk.intro':
    'Put one account on each line, or choose a CSV file. The first line lists the columns and is filled in for you: keep it. We log in to the accounts one at a time, so this can take a minute.',
  'accounts.bulk.columns':
    'Columns, in this order: mail server, port, username, password, From address, From name, daily limit. Write all seven on every line: to leave port, From name or daily limit empty, keep its comma. A password with a comma in it goes in double quotes.',
  'accounts.bulk.badFile':
    'We could not read the accounts. Every line needs all seven values separated by commas, and the first line has to stay as it is. Nothing was added.',
  'accounts.bulk.lines': 'Accounts',
  'accounts.bulk.header': 'Mail server,Port,Username,Password,From address,From name,Daily limit',
  'accounts.bulk.file': 'Or a CSV file',
  'accounts.bulk.fileHint': 'It needs the same first line. When you choose a file, the lines above are not used.',
  'accounts.bulk.submit': 'Add these accounts',
  'accounts.bulk.working': 'Adding and checking the accounts. This can take a minute.',
  'accounts.bulk.nothing': 'Paste your accounts or choose a file first.',
  'accounts.bulk.badLine': 'Line {line} has a problem: {fields}. Nothing was added. Fix the line and try again.',
  'accounts.bulk.badLinePlain': 'Line {line} is not valid. Nothing was added. Fix the line and try again.',
  'accounts.bulk.summary': 'Added: {added}. Not added: {failed}.',
  'accounts.bulk.summaryCheck': 'Look at the list for the sending address check of each new account.',
  'accounts.bulk.caption': 'Result for each line',
  'accounts.bulk.colLine': 'Line',
  'accounts.bulk.colResult': 'Result',
  'accounts.bulk.colDetails': 'What to do',
  'accounts.bulk.created': 'Added',
  'accounts.bulk.createdUnverified': 'Added, login not verified',
  'accounts.bulk.duplicate': 'Duplicate',
  'accounts.bulk.ackRequired': 'Needs your confirmation',
  'accounts.bulk.hostForbidden': 'Server not allowed',
  'accounts.bulk.limitReached': 'Limit reached',
  'accounts.bulk.invalid': 'Invalid line',
  'accounts.bulk.unavailable': 'Not available',
  'accounts.bulk.failed': 'Not added',
  'accounts.bulk.createdUnverifiedWhy': 'The account exists, but the login could not be verified. Use Test on it in the list.',
  'accounts.bulk.ackRequiredWhy': 'The From address has a warning. Add this account on its own to read the warning and confirm it.',
  'accounts.bulk.invalidWhy': 'Something on this line is not valid. Check the values and add this account on its own to see which.',
  'accounts.bulk.failedWhy': 'This line could not be processed. Try it again on its own.',

  // ---- the names of the fields of an account, for a message that points at one ----
  'accounts.field.server': 'mail server',
  'accounts.field.port': 'port',
  'accounts.field.username': 'username',
  'accounts.field.password': 'password',
  'accounts.field.fromAddress': 'From address',
  'accounts.field.fromName': 'From name',
  'accounts.field.dailyLimit': 'daily limit',
  'accounts.field.other': 'a value',

  // ---- how long to wait (a rate limit says so) ----
  'accounts.wait.second': 'You can try again in a second.',
  'accounts.wait.seconds': 'You can try again in {count} seconds.',
  'accounts.wait.minute': 'You can try again in about a minute.',
  'accounts.wait.minutes': 'You can try again in about {count} minutes.',
  'accounts.wait.hour': 'You can try again in about an hour.',
  'accounts.wait.hours': 'You can try again in about {count} hours.',
} as const satisfies AreaCopy<'accounts'>
