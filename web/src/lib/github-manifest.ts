/**
 * Build the GitHub App manifest POST URL with `state` set exactly once.
 * BeginManifestSetup may already include `?state=` in postUrl; naively
 * appending another `?state=` produces a mangled token GitHub echoes back,
 * which then fails the callback with "unknown or already used state".
 */
export function githubManifestPostAction(postUrl: string, state: string): string {
  const u = new URL(postUrl)
  u.searchParams.set('state', state)
  return u.toString()
}
