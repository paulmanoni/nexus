package pubsub

// Public introspection surface. Reads the process-global registry —
// safe to call any time after init, returns sorted snapshots that
// the caller may freely mutate.
//
// Subscriptions also surface as workers via the AsWorker
// registration in Subscribe, so the dashboard's existing workers
// panel already lists them at "pubsub:<topic>:<subscription>". These
// helpers exist for callers that want the structured view (a future
// dashboard widget, a /__nexus/pubsub HTTP endpoint, an audit
// script): one record per topic with its declared payload type, one
// record per subscription with its retry knobs.

// TopicInfo is a snapshot of one registered topic. Built from the
// internal topicRecord; PayloadType is the Go type name (e.g.
// "main.UserCreatedEvent") so a JSON consumer can read it without
// reflection.
type TopicInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Durable     bool   `json:"durable,omitempty"`
	PayloadType string `json:"payloadType,omitempty"`
}

// SubscriptionInfo is a snapshot of one registered subscription. The
// dispatch tuning is included so an operator can answer "why did this
// message DLQ" without reading source.
type SubscriptionInfo struct {
	Topic         string `json:"topic"`
	Name          string `json:"name"`
	MaxRetries    int    `json:"maxRetries,omitempty"`
	AckDeadlineMs int64  `json:"ackDeadlineMs,omitempty"`
}

// Topics returns every topic registered via NewTopic, sorted by name.
// Safe to call from any goroutine; returns a fresh slice the caller
// may mutate.
func Topics() []TopicInfo {
	src := snapshotTopics()
	out := make([]TopicInfo, 0, len(src))
	for _, t := range src {
		var pt string
		if rt := t.PayloadType(); rt != nil {
			pt = rt.String()
		}
		out = append(out, TopicInfo{
			Name:        t.Name(),
			Description: t.Description(),
			Durable:     t.Durable(),
			PayloadType: pt,
		})
	}
	return out
}

// Subscriptions returns every subscription registered via Subscribe,
// sorted by (topic, subscription name).
func Subscriptions() []SubscriptionInfo {
	src := snapshotSubscriptions()
	out := make([]SubscriptionInfo, 0, len(src))
	for _, s := range src {
		out = append(out, SubscriptionInfo{
			Topic:         s.Topic,
			Name:          s.Name,
			MaxRetries:    s.MaxRetries,
			AckDeadlineMs: s.AckDeadlinMs,
		})
	}
	return out
}