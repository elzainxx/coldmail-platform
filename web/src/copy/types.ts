// The shape every copy file has: a flat object whose keys all start with the file's own area and a dot.
// A screen agent adds entries to its own area file only; the keys are then a type (CopyKey in index.ts) and a
// missing key is a compile error. Values are plain sentences; {name} marks a value filled in by t(key, { name }).

/** `AreaCopy<'jobs'>` is an object with keys `jobs.<anything>` and string values. */
export type AreaCopy<P extends string> = Record<`${P}.${string}`, string>
