// Package semconv defines the attribute keys EffectTrace reads and writes.
//
// Keys in the effecttrace.* namespace are project-specific and documented in
// docs/semantic-conventions.md. Keys from OpenTelemetry semantic conventions
// are repeated here as constants because the GenAI/MCP conventions are in
// Development status and are no longer generated in the newest
// go.opentelemetry.io/otel/semconv packages.
package semconv

// OpenTelemetry MCP / GenAI / JSON-RPC conventions (Development status).
const (
	MCPMethodName      = "mcp.method.name"
	MCPProtocolVersion = "mcp.protocol.version"
	GenAIToolName      = "gen_ai.tool.name"
	GenAIOperationName = "gen_ai.operation.name"
	JSONRPCRequestID   = "jsonrpc.request.id"
	ErrorType          = "error.type"
	NetworkTransport   = "network.transport"
	NetworkProtoName   = "network.protocol.name"

	// MCPMethodToolsCall is the mcp.method.name value of a tool invocation.
	MCPMethodToolsCall = "tools/call"
	// GenAIOperationExecuteTool is the gen_ai.operation.name of a tool call.
	GenAIOperationExecuteTool = "execute_tool"
	// ErrorTypeToolError marks a CallToolResult with isError=true.
	ErrorTypeToolError = "tool_error"
)

// Stable OpenTelemetry HTTP and resource conventions used by EffectTrace.
const (
	HTTPRequestMethod      = "http.request.method"
	HTTPResponseStatusCode = "http.response.status_code"
	ServiceName            = "service.name"
)

// Kubernetes request attributes recorded on instrumented client spans. They
// describe the TARGET of an API request, which is why they do not reuse the
// k8s.* resource attributes (those describe the entity producing telemetry).
const (
	K8sAuditID               = "effecttrace.k8s.audit_id"
	K8sVerb                  = "effecttrace.k8s.verb"
	K8sAPIGroup              = "effecttrace.k8s.api_group"
	K8sResource              = "effecttrace.k8s.resource"
	K8sSubresource           = "effecttrace.k8s.subresource"
	K8sObjectNamespace       = "effecttrace.k8s.object.namespace"
	K8sObjectName            = "effecttrace.k8s.object.name"
	K8sObjectKind            = "effecttrace.k8s.object.kind"
	K8sObjectUID             = "effecttrace.k8s.object.uid"
	K8sObjectResourceVersion = "effecttrace.k8s.object.resource_version"
	K8sObjectGeneration      = "effecttrace.k8s.object.generation"
)

// Attributes on spans EffectTrace exports for completed effect graphs.
const (
	GraphID          = "effecttrace.graph.id"
	GraphStatus      = "effecttrace.graph.status"
	ActionID         = "effecttrace.action.id"
	ActionKind       = "effecttrace.action.kind"
	NodeType         = "effecttrace.node.type"
	NodeID           = "effecttrace.node.id"
	NodeGrade        = "effecttrace.node.grade"
	EdgeRelationship = "effecttrace.edge.relationship"
	EvidenceType     = "effecttrace.evidence.type"
	EdgeReason       = "effecttrace.edge.reason"
	K8sUID           = "effecttrace.k8s.uid"
	WindowName       = "effecttrace.observation.window.name"
	WindowStart      = "effecttrace.observation.window.start"
	WindowEnd        = "effecttrace.observation.window.end"
	EdgeCountPrefix  = "effecttrace.edges." // + lowercase evidence type
)
