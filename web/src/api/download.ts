// Hand a Blob to the browser as a file download (the CSV of a job's results is fetched with the Authorization header, so it cannot be a plain link).
export function saveBlob(blob: Blob, filename: string, doc: Document = document): void {
  const url = URL.createObjectURL(blob)
  const a = doc.createElement('a')
  a.href = url
  a.download = filename
  a.rel = 'noopener'
  doc.body.appendChild(a)
  a.click()
  a.remove()
  // the browser has started the download by the time a task later; revoking sooner can cancel it
  setTimeout(() => URL.revokeObjectURL(url), 10_000)
}
