import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Badge } from './badge'
import { NotAvailable } from './states'
import { ConnectError, Code } from '@connectrpc/connect'
import { QueryError } from './states'

describe('ui states', () => {
  it('renders badges', () => {
    render(<Badge tone="success">up</Badge>)
    expect(screen.getByText('up')).toBeInTheDocument()
  })
  it('explains Unimplemented RPCs gracefully', () => {
    render(<QueryError error={new ConnectError('nope', Code.Unimplemented)} what="users" />)
    expect(screen.getByText(/not available on this node yet/)).toBeInTheDocument()
  })
  it('shows real errors', () => {
    render(<QueryError error={new ConnectError('boom', Code.Internal)} what="users" />)
    expect(screen.getByText(/Couldn't load users/)).toBeInTheDocument()
    expect(screen.getByText('boom')).toBeInTheDocument()
  })
  it('NotAvailable names the feature', () => {
    render(<NotAvailable what="GitHub" />)
    expect(screen.getByText(/GitHub is not available/)).toBeInTheDocument()
  })
})
