// _nl-react-runtime.ts — shared once across all React
// islands in a project. Provides the channel-context
// plumbing so each island doesn't have to reinvent it.
//
// Underscore-prefixed so Vite's *.island.tsx glob doesn't
// pick this up as an entry point.

import { createContext, useContext } from 'react'

export type Channel = {
  /** Subscribe to a server-pushed event. Returns the unsubscribe func. */
  on(event: string, fn: (payload: any) => void): () => void
}

// Default value is a no-op channel so a missing Provider
// doesn't crash — just silently drops messages. Useful if
// you want to render a React island outside the bridge for
// testing (Storybook, etc.).
export const ChannelContext = createContext<Channel>({
  on: () => () => undefined,
})

/** Convenience hook so call sites don't repeat the useContext dance. */
export const useChannel = (): Channel => useContext(ChannelContext)
