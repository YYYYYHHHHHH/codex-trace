package trace

// Langfuse 识别的 OTel 属性名（来源：langfuse 仓库 packages/shared/src/server/otel/attributes.ts）。
// 集中在此处，M1 用真实 Langfuse 验证后如需调整只改这里。
const (
	attrTraceName     = "langfuse.trace.name"
	attrTraceTags     = "langfuse.trace.tags"
	attrTraceInput    = "langfuse.trace.input"
	attrTraceOutput   = "langfuse.trace.output"
	attrTraceMetaPref = "langfuse.trace.metadata."
	attrSessionID     = "session.id"
	attrUserID        = "user.id"
	attrEnvironment   = "langfuse.environment"
	attrRelease       = "langfuse.release"

	attrObsType       = "langfuse.observation.type"
	attrObsLevel      = "langfuse.observation.level"
	attrObsStatusMsg  = "langfuse.observation.status_message"
	attrObsInput      = "langfuse.observation.input"
	attrObsOutput     = "langfuse.observation.output"
	attrObsMetaPref   = "langfuse.observation.metadata."
	attrObsModel      = "langfuse.observation.model.name"
	attrObsModelParam = "langfuse.observation.model.parameters"
	attrObsUsage      = "langfuse.observation.usage_details"

	// OTel GenAI 语义约定，作为兜底。
	attrGenAIModel        = "gen_ai.request.model"
	attrGenAIInputTokens  = "gen_ai.usage.input_tokens"
	attrGenAIOutputTokens = "gen_ai.usage.output_tokens"
)

const (
	obsAgent      = "agent"
	obsTool       = "tool"
	obsGeneration = "generation"

	levelWarning = "WARNING"
	levelError   = "ERROR"
)

// span id 的种类前缀；同一业务 id 在不同种类下得到不同 span id。
const (
	KindTurn       = "turn"
	KindTool       = "tool"
	KindGeneration = "gen"
)
