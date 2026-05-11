// Shared time formatters used across views. Three near-identical
// helpers were inlined per-file before this — pulling them here means
// "5m ago" looks the same in Crons, Auth, and the error dialog.

// formatAbsolute renders a timestamp as a locale datetime in 24h. Falsy
// inputs render as an em-dash so tables stay aligned; parse failures
// fall back to the original string so the value is still debuggable.
export function formatAbsolute(t) {
  if (!t) return '—'
  try { return new Date(t).toLocaleString([], { hour12: false }) } catch { return String(t) }
}

// formatTime renders only the time portion (HH:MM:SS, 24h). Used in
// dense streams (Traces) where the date repeats across rows and adds
// no info.
export function formatTime(t) {
  try { return new Date(t).toLocaleTimeString([], { hour12: false }) } catch { return '' }
}

// formatRelative renders the delta from now as e.g. "in 5m" or "3s
// ago". Picks the largest reasonable unit (s/m/h/d). Returns '' for
// falsy. Accepts anything Date() parses — number (epoch ms), string
// (ISO), or Date.
export function formatRelative(t) {
  if (!t) return ''
  const ms = new Date(t).getTime()
  if (!ms) return String(t)
  const delta = ms - Date.now()
  return delta >= 0
    ? `in ${spanLabel(delta)}`
    : `${spanLabel(-delta)} ago`
}

// formatDuration renders a positive ms span at sub-second resolution:
// "<1ms" / "123ms" / "1.23s". Used for trace span durations where
// sub-second precision matters; rolls into "1.23s" rather than "1s"
// so callers can distinguish 1.0s from 1.4s. For wall-clock relative
// time use formatRelative.
export function formatDuration(ms) {
  if (ms == null) return ''
  if (ms < 1) return '<1ms'
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

// spanLabel renders an absolute ms span as the largest reasonable
// unit, rounded. Internal helper for formatRelative.
function spanLabel(ms) {
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.round(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h`
  return `${Math.round(h / 24)}d`
}