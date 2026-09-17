/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { type ReactNode, useEffect } from 'react'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  type Mock,
  test,
  vi,
} from 'vitest'

import { api } from '@/lib/api'

import { ChannelsProvider, useChannels } from '../../channels-provider'
import { EditTagDialog } from '../edit-tag-dialog'

/**
 * Regression coverage for the tag-edit dialog silently overwriting every
 * channel in a tag with one arbitrary model. Since the 2026-09-17 per-model
 * channel split, channels sharing a tag normally each carry a single,
 * different model, so the dialog must only send `models` in the save
 * request when the operator explicitly edited it in this session.
 */

type ApiMethod = (
  url: string,
  data?: unknown,
  config?: unknown
) => Promise<{ data: unknown }>
type MockableApi = {
  get: ApiMethod
  put: ApiMethod
}

const apiClient = api as unknown as MockableApi
const originalGet = apiClient.get
const originalPut = apiClient.put
const originalGetAnimations = Object.getOwnPropertyDescriptor(
  HTMLElement.prototype,
  'getAnimations'
)

const queryClients: QueryClient[] = []

function createQueryClient(): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  queryClients.push(queryClient)
  return queryClient
}

function WithTag({ tag, children }: { tag: string; children: ReactNode }) {
  const { setCurrentTag } = useChannels()
  useEffect(() => {
    setCurrentTag(tag)
  }, [tag, setCurrentTag])
  return children
}

function renderDialog(tag: string): ReturnType<typeof render> {
  return render(
    <QueryClientProvider client={createQueryClient()}>
      <ChannelsProvider>
        <WithTag tag={tag}>
          <EditTagDialog open onOpenChange={() => undefined} />
        </WithTag>
      </ChannelsProvider>
    </QueryClientProvider>
  )
}

function lastPutPayload(): Record<string, unknown> {
  const calls = (apiClient.put as Mock).mock.calls
  return calls.at(-1)?.[1] as Record<string, unknown>
}

beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, 'getAnimations', {
    configurable: true,
    value: () => [],
  })
})

afterAll(() => {
  if (originalGetAnimations) {
    Object.defineProperty(
      HTMLElement.prototype,
      'getAnimations',
      originalGetAnimations
    )
    return
  }
  Reflect.deleteProperty(HTMLElement.prototype, 'getAnimations')
})

beforeEach(() => {
  apiClient.get = vi.fn(async (url: string) => {
    if (url === '/api/channel/tag/models') {
      return {
        data: {
          success: true,
          data: 'claude-3-opus,gpt-4o',
          channel_count: 2,
        },
      }
    }
    if (url === '/api/channel/models') {
      return { data: { success: true, data: [] } }
    }
    if (url === '/api/group/') {
      return { data: { success: true, data: ['default'] } }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
  apiClient.put = vi.fn(async () => ({ data: { success: true } }))
})

afterEach(() => {
  cleanup()
  queryClients.splice(0).forEach((client) => client.clear())
  apiClient.get = originalGet
  apiClient.put = originalPut
})

test('saving without touching models leaves every channel of the tag untouched', async () => {
  const user = userEvent.setup()
  renderDialog('prod-pool')

  await screen.findByText('2 channel(s) currently share this tag')
  const tagInput = screen.getByPlaceholderText(
    'Enter new tag name or leave empty'
  )
  await waitFor(() => expect(tagInput).toHaveValue('prod-pool'))
  await user.clear(tagInput)
  await user.type(tagInput, 'prod-pool-renamed')
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))

  await waitFor(() => expect(apiClient.put).toHaveBeenCalled())
  const payload = lastPutPayload()
  expect(payload).not.toHaveProperty('models')
  expect(payload).toMatchObject({
    tag: 'prod-pool',
    new_tag: 'prod-pool-renamed',
  })
})

test('explicitly adding a model applies it as a tag-wide override', async () => {
  const user = userEvent.setup()
  renderDialog('prod-pool')

  await screen.findByText('2 channel(s) currently share this tag')
  await user.type(
    screen.getByPlaceholderText('Custom model (comma-separated)'),
    'new-model'
  )
  await user.click(screen.getByRole('button', { name: 'Add' }))
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))

  await waitFor(() => expect(apiClient.put).toHaveBeenCalled())
  expect(lastPutPayload()).toMatchObject({
    tag: 'prod-pool',
    models: 'new-model',
  })
})

test('starting from current models seeds the editable list and applies it on save', async () => {
  const user = userEvent.setup()
  renderDialog('prod-pool')

  await screen.findByText('2 channel(s) currently share this tag')
  await user.click(
    screen.getByRole('button', { name: 'Start from current models' })
  )
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))

  await waitFor(() => expect(apiClient.put).toHaveBeenCalled())
  expect(lastPutPayload()).toMatchObject({
    tag: 'prod-pool',
    models: 'claude-3-opus,gpt-4o',
  })
})
