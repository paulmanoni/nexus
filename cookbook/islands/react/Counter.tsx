// Counter.tsx — reference nl-island authored in React 18.
//
// useChannel() is the only nexus-specific touchpoint;
// everything else is plain React.

import { useState, useEffect } from 'react'
import { useChannel } from './_nl-react-runtime'

type Props = {
  initial?: number
}

export default function Counter({ initial = 0 }: Props) {
  const [count, setCount] = useState(initial)
  const channel = useChannel()

  // Server can fire ctx.PushIsland("Counter", "reset", nil)
  // to zero the count without remounting the component —
  // useState's value survives because mount() never re-runs.
  useEffect(() => {
    const off = channel.on('reset', () => setCount(0))
    return off
  }, [channel])

  return (
    <div className="counter-island">
      <button onClick={() => setCount((c) => c + 1)}>
        Count: {count}
      </button>
      <p className="hint">
        Click to bump on the client only.
        Server can fire <code>PushIsland("Counter", "reset", nil)</code>
        to zero this back out.
      </p>
    </div>
  )
}
