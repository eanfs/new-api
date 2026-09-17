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
import { act, render, screen, waitFor } from '@testing-library/react'
import { StrictMode } from 'react'
import { Toaster } from 'sonner'
import { expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { Wallet } from '..'
import type { AntomOrderStatusResponse } from '../types'

test.each([true, false])(
  'consumes a delayed Antom return once in StrictMode (inquiry accepted: %s)',
  async (accepted) => {
    let resolveInquiry!: (response: AntomOrderStatusResponse) => void
    const inquiry = new Promise<AntomOrderStatusResponse>((resolve) => {
      resolveInquiry = resolve
    })
    let settled = false
    const consumed = vi.fn()
    const subscription = {
      subscription: {
        id: 42,
        user_id: 1,
        plan_id: 1,
        status: 'active',
        start_time: 1700000000,
        end_time: 4102444800,
        amount_total: 500000,
        amount_used: 0,
      },
    }
    const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/user/antom/orders/antom-return') {
        return { data: await inquiry }
      }
      let data: unknown
      switch (url.split('?')[0]) {
        case '/api/status':
          data = { quota_per_unit: 500000, quota_display_type: 'USD' }
          break
        case '/api/user/self':
          data = {
            id: 1,
            username: 'buyer',
            quota: settled ? 1000000 : 500000,
            used_quota: 0,
            request_count: 0,
            aff_count: 0,
            aff_quota: 0,
            aff_history: 0,
          }
          break
        case '/api/user/topup/info':
          data = { pay_methods: [], amount_options: [], discount: {} }
          break
        case '/api/subscription/plans':
          data = []
          break
        case '/api/subscription/self':
          data = {
            billing_preference: 'wallet_first',
            subscriptions: settled ? [subscription] : [],
            all_subscriptions: settled ? [subscription] : [],
          }
          break
        case '/api/user/aff':
          data = 'buyer'
          break
        case '/api/user/topup/self':
          data = { items: [], total: 0 }
          break
        default:
          throw new Error(`Unexpected request: ${url}`)
      }
      return { data: { success: true, data } }
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const view = render(
      <StrictMode>
        <QueryClientProvider client={client}>
          <Wallet antomOrder='antom-return' onAntomOrderConsumed={consumed} />
          <Toaster />
        </QueryClientProvider>
      </StrictMode>
    )

    try {
      expect(await screen.findByText(/^\$1(?:\.00)?$/)).toBeVisible()
      expect(consumed).not.toHaveBeenCalled()
      expect(screen.queryByText('Payment completed')).not.toBeInTheDocument()

      await act(async () => {
        settled = accepted
        resolveInquiry({
          success: accepted,
          message: accepted ? undefined : 'Order lookup rejected',
          // Even an incidental success status must not override a rejected API response.
          data: { status: 'success' },
        })
      })
      await waitFor(() => expect(consumed).toHaveBeenCalledTimes(1))
      expect(
        get.mock.calls.filter(
          ([url]) => url === '/api/user/antom/orders/antom-return'
        )
      ).toHaveLength(1)

      if (accepted) {
        expect(await screen.findByText('Payment completed')).toBeVisible()
        expect(await screen.findByText(/^\$2(?:\.00)?$/)).toBeVisible()
        expect(await screen.findByText('Subscription #42')).toBeVisible()
      } else {
        expect(await screen.findByText('Order lookup rejected')).toBeVisible()
        expect(screen.queryByText('Payment completed')).not.toBeInTheDocument()
        expect(screen.getByText(/^\$1(?:\.00)?$/)).toBeVisible()
        expect(screen.queryByText('Subscription #42')).not.toBeInTheDocument()
      }
    } finally {
      view.unmount()
      client.clear()
    }
  }
)
