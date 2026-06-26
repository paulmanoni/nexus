package nexus

// Describe sets an endpoint's human-readable description — shown on the
// introspection dashboard (/__nexus) and, for GraphQL, emitted into the
// generated SDL documentation. Cross-transport: one expression works on REST
// (AsRest / AsRestHandler), GraphQL (AsQuery / AsMutation), and WS (AsWS),
// mirroring HideFromDashboard / WithIcon.
//
//	nexus.AsRest("POST", "/devices", NewRegister, nexus.Describe("Register a device"))
//	nexus.AsQuery(NewSearchUsers, nexus.Describe("Full-text user search"))
//	nexus.AsWS("/events", "chat.send", NewChatSend, nexus.Describe("Send a chat message"))
//
// Describe supersedes the transport-specific Desc (GraphQL) and Description
// (REST) helpers, which remain for compatibility.
func Describe(s string) DescribeOption { return DescribeOption{text: s} }

// DescribeOption is the cross-transport carrier returned by Describe — it
// implements RestOption, GqlOption, and WSOption so one value flows through any
// endpoint builder, mirroring IconOption and DashboardHiddenOption. It sets the
// shared baseEndpointConfig.description that registerEndpoint stamps onto the
// registry entry for every transport.
type DescribeOption struct{ text string }

func (o DescribeOption) applyToRest(c *restConfig) { c.description = o.text }
func (o DescribeOption) applyToGql(c *gqlConfig)   { c.description = o.text }
func (o DescribeOption) applyToWS(c *wsConfig)     { c.description = o.text }
