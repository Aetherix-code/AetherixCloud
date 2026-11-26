import { describe, expect, it } from 'vitest'
import { githubManifestPostAction } from './github-manifest'

describe('githubManifestPostAction', () => {
  it('adds state when the API postUrl has none (mock)', () => {
    expect(githubManifestPostAction('https://github.com/settings/apps/new', 'abc')).toBe(
      'https://github.com/settings/apps/new?state=abc',
    )
  })

  it('does not double-append when postUrl already has state (real API)', () => {
    const post = 'https://github.com/settings/apps/new?state=abc'
    expect(githubManifestPostAction(post, 'abc')).toBe('https://github.com/settings/apps/new?state=abc')
  })

  it('replaces a mismatched state query rather than appending a second ?', () => {
    // Regression: `${postUrl}?state=` turned into state=abc?state=abc
    const broken = 'https://github.com/organizations/octo/settings/apps/new?state=tok'
    expect(githubManifestPostAction(broken, 'tok')).toBe(
      'https://github.com/organizations/octo/settings/apps/new?state=tok',
    )
    expect(githubManifestPostAction(broken, 'tok')).not.toContain('?state=tok?state=')
  })
})
