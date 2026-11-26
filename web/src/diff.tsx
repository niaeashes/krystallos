import { Fragment, useState } from 'react'
import type { FileDiff, ReviewComment } from './api'
import { Identity } from './ui'

interface Row {
  kind: 'hunk' | 'add' | 'del' | 'ctx' | 'note'
  text: string
  oldNo?: number
  newNo?: number
}

// parsePatch reads one file's unified diff into rows with line numbers.
export function parsePatch(patch: string): Row[] {
  const rows: Row[] = []
  let oldNo = 0
  let newNo = 0
  let inHunk = false
  for (const line of patch.split('\n')) {
    if (line.startsWith('@@')) {
      const m = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line)
      if (m) {
        oldNo = Number(m[1])
        newNo = Number(m[2])
        inHunk = true
        rows.push({ kind: 'hunk', text: line })
      }
      continue
    }
    if (!inHunk) continue
    if (line.startsWith('+')) rows.push({ kind: 'add', text: line.slice(1), newNo: newNo++ })
    else if (line.startsWith('-')) rows.push({ kind: 'del', text: line.slice(1), oldNo: oldNo++ })
    else if (line.startsWith('\\')) rows.push({ kind: 'note', text: line })
    else rows.push({ kind: 'ctx', text: line.slice(1), oldNo: oldNo++, newNo: newNo++ })
  }
  return rows
}

export interface PlacedComment extends ReviewComment {
  author?: string
  pending?: boolean
}

function anchor(row: Row): { side: 'new' | 'old'; line: number } | null {
  if (row.kind === 'del' && row.oldNo) return { side: 'old', line: row.oldNo }
  if ((row.kind === 'add' || row.kind === 'ctx') && row.newNo) return { side: 'new', line: row.newNo }
  return null
}

const statusLabel: Record<string, string> = { A: 'added', D: 'deleted', M: 'modified', R: 'renamed', C: 'copied', T: 'type changed' }

export function DiffView({
  files,
  comments,
  onComment,
}: {
  files: FileDiff[]
  comments: PlacedComment[]
  onComment?: (c: ReviewComment) => void
}) {
  if (files.length === 0) return <div className="empty">No changes.</div>
  return (
    <>
      {files.map((f) => (
        <FileBlock key={f.path} file={f} comments={comments.filter((c) => c.path === f.path)} onComment={onComment} />
      ))}
    </>
  )
}

function FileBlock({ file, comments, onComment }: { file: FileDiff; comments: PlacedComment[]; onComment?: (c: ReviewComment) => void }) {
  const [draft, setDraft] = useState<{ side: 'new' | 'old'; line: number; body: string } | null>(null)
  const rows = parsePatch(file.patch)
  return (
    <div className="file">
      <div className="file-head">
        <span>
          {file.old_path ? `${file.old_path} → ` : ''}
          {file.path}
        </span>
        <span className="muted small">
          {statusLabel[file.status] ?? file.status}
          {!file.binary && (
            <>
              {' '}
              <span className="add">+{file.additions}</span> <span className="del">−{file.deletions}</span>
            </>
          )}
        </span>
      </div>
      {file.binary ? (
        <div className="empty">Binary file.</div>
      ) : (
        <table className="diff">
          <colgroup>
            <col className="diff-col-ln" />
            <col className="diff-col-ln" />
            <col />
          </colgroup>
          <tbody>
            {rows.map((row, i) => {
              const at = anchor(row)
              const here = at ? comments.filter((c) => c.side === at.side && c.line === at.line) : []
              const drafting = at && draft && draft.side === at.side && draft.line === at.line
              return (
                <Fragment key={i}>
                  <tr className={`diff-${row.kind}`}>
                    {row.kind === 'hunk' ? (
                      <td colSpan={3} className="diff-hunk-text">
                        {row.text}
                      </td>
                    ) : (
                      <>
                        <td className="ln">{row.oldNo ?? ''}</td>
                        <td className="ln">
                          {onComment && at ? (
                            <button className="ln-button" title="Comment on this line" onClick={() => setDraft({ ...at, body: '' })}>
                              {row.newNo ?? row.oldNo}
                            </button>
                          ) : (
                            (row.newNo ?? '')
                          )}
                        </td>
                        <td className="diff-text">
                          <span className="diff-sign">{row.kind === 'add' ? '+' : row.kind === 'del' ? '−' : ' '}</span>
                          {row.text}
                        </td>
                      </>
                    )}
                  </tr>
                  {here.map((c, j) => (
                    <tr key={`c${j}`} className="diff-comment-row">
                      <td colSpan={3}>
                        <div className={`inline-comment${c.pending ? ' inline-comment-pending' : ''}`}>
                          {c.author ? <Identity handle={c.author} /> : <span className="muted small">pending</span>}
                          <span>{c.body}</span>
                        </div>
                      </td>
                    </tr>
                  ))}
                  {drafting && draft && (
                    <tr className="diff-comment-row">
                      <td colSpan={3}>
                        <form
                          className="inline-comment-form"
                          onSubmit={(e) => {
                            e.preventDefault()
                            if (!draft.body.trim()) return
                            onComment?.({ path: file.path, side: draft.side, line: draft.line, body: draft.body })
                            setDraft(null)
                          }}
                        >
                          <textarea value={draft.body} onChange={(e) => setDraft({ ...draft, body: e.target.value })} rows={2} autoFocus />
                          <div className="form-actions">
                            <button className="button button-primary">Add to review</button>
                            <button type="button" className="button button-quiet" onClick={() => setDraft(null)}>
                              Cancel
                            </button>
                          </div>
                        </form>
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      )}
      {file.truncated && <div className="muted small">This file's diff was cut short.</div>}
    </div>
  )
}
