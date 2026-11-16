import { useTranslation } from 'react-i18next'
import { useMe } from '../api/hooks'
import { useViewerChange, useViewers, type ViewerKind } from '../api/viewers'
import { formatDateTime } from '../format'
import { errorMessage } from '../i18n'
import { Alert, Badge, Button, Card, SectionTitle } from './ui'

const code = (season: number, episode: number) => `S${String(season).padStart(2, '0')}E${String(episode).padStart(2, '0')}`

/** Who watches a movie or series, with Join/Leave for the signed-in user. */
export function Viewers({ kind, id }: { kind: ViewerKind; id: number }) {
  const { t, i18n } = useTranslation()
  const { data: me } = useMe()
  const list = useViewers(kind, id)
  const change = useViewerChange(kind, id)
  const viewers = list.data?.viewers ?? []
  const joined = !!me && viewers.some((v) => v.userId === me.id)

  return (
    <Card className="space-y-3">
      <SectionTitle
        title={t('viewers.title')}
        description={t(kind === 'movies' ? 'viewers.descriptionMovie' : 'viewers.descriptionSeries')}
        actions={
          me && (
            <Button variant="secondary" loading={change.isPending} onClick={() => change.mutate({ action: joined ? 'leave' : 'join' })}>
              {joined ? t('viewers.leave') : t('viewers.join')}
            </Button>
          )
        }
      />
      {change.isError && <Alert>{errorMessage(t, change.error)}</Alert>}
      {list.isError && <Alert>{errorMessage(t, list.error)}</Alert>}
      {list.data?.holdUntil && <p className="text-sm text-muted">{t('viewers.held', { date: formatDateTime(list.data.holdUntil, i18n.resolvedLanguage) })}</p>}
      {viewers.length === 0 ? (
        <p className="text-sm text-muted">{t('viewers.none')}</p>
      ) : (
        <ul className="divide-y divide-line">
          {viewers.map((v) => (
            <li key={v.userId} className="flex flex-wrap items-center gap-2 py-2 text-sm">
              <span className="font-medium">{v.userName || t('viewers.unknownUser')}</span>
              <Badge tone="muted">{t(`viewers.sources.${v.source}`)}</Badge>
              {v.holding && <Badge tone="warn">{t('viewers.holding')}</Badge>}
              <span className="ml-auto text-muted">
                {kind === 'series'
                  ? v.season
                    ? t('viewers.position', { code: code(v.season, v.episode ?? 0), count: v.window ?? 0 })
                    : t('viewers.done')
                  : v.watchedAt
                    ? t('viewers.watched')
                    : v.progress > 0
                      ? t('viewers.progress', { percent: Math.round(v.progress * 100) })
                      : t('viewers.notStarted')}
              </span>
              {me?.role === 'admin' && v.userId !== me.id && (
                <Button variant="ghost" onClick={() => change.mutate({ action: 'remove', userId: v.userId })}>
                  {t('viewers.remove')}
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
