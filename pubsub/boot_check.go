package pubsub

import (
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/manifest"
)

// init registers pubsub's boot self-check. Only linked (and thus only run) when
// an app imports pubsub — the pay-for-what-you-use pattern that keeps the nexus
// core free of a pubsub dependency.
func init() { nexus.RegisterBootCheck(bootCheck) }

// bootCheck promotes the classic pubsub foot-gun from first-Publish (possibly at
// 2am) to a loud report at boot in dev: topics were declared but no transport
// was ever bound (the app forgot pubsub.UseInMemory() / pubsub.UseRabbit(...)).
// It runs LAST among boot invokes (see nexus.Run), after BindTopics, so a bound
// transport is already visible and this doesn't false-positive. Returns nil when
// there are no topics or a transport is bound.
func bootCheck() []manifest.Issue {
	topics := snapshotTopics()
	if len(topics) == 0 {
		return nil
	}
	regMu.RLock()
	bound := activeTransport != nil
	regMu.RUnlock()
	if bound {
		return nil
	}
	names := make([]string, 0, len(topics))
	for _, t := range topics {
		names = append(names, t.Name())
	}
	return []manifest.Issue{{
		Severity: manifest.SeverityError,
		Code:     manifest.ErrCode("PUBSUB_NO_TRANSPORT"),
		Path:     "pubsub",
		Message: fmt.Sprintf(
			"%d topic(s) declared (%v) but no transport is bound — add pubsub.UseInMemory() or "+
				"pubsub.UseRabbit(...) to nexus.Run/Boot; Publish will fail with \"no transport bound\" until you do",
			len(topics), names),
	}}
}
