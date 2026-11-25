import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import './index.css'
import { initApi } from '@/lib/api'
import { AuthProvider } from '@/lib/auth'
import { Toaster } from '@/components/ui/toast'
import { router } from './router'

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 4000, retry: 1, refetchOnWindowFocus: true } },
})

initApi().then(() => {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <AuthProvider>
          <RouterProvider router={router} />
          <Toaster />
        </AuthProvider>
      </QueryClientProvider>
    </StrictMode>,
  )
})
