import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from './client'
import type { ViewerList } from './types'

export type ViewerKind = 'movies' | 'series'

export const viewerKeys = {
  list: (kind: ViewerKind, id: number) => ['viewers', kind, id] as const,
}

/** The users who watch a movie or series. */
export function useViewers(kind: ViewerKind, id: number) {
  return useQuery({
    queryKey: viewerKeys.list(kind, id),
    queryFn: ({ signal }) => api<ViewerList>(`/${kind}/${id}/viewers`, { signal }),
  })
}

/** Join or leave as viewer; administrators can also remove someone. */
export function useViewerChange(kind: ViewerKind, id: number) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (change: { action: 'join' } | { action: 'leave' } | { action: 'remove'; userId: number }) => {
      switch (change.action) {
        case 'join':
          return api<ViewerList>(`/${kind}/${id}/watch`, { method: 'POST' })
        case 'leave':
          return api<ViewerList>(`/${kind}/${id}/watch`, { method: 'DELETE' })
        default:
          return api<ViewerList>(`/${kind}/${id}/viewers/${change.userId}`, { method: 'DELETE' })
      }
    },
    onSuccess: (list) => {
      qc.setQueryData(viewerKeys.list(kind, id), list)
      void qc.invalidateQueries({ queryKey: [kind === 'movies' ? 'movies' : 'series'] })
    },
  })
}
