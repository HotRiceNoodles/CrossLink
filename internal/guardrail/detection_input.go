package guardrail

import (
	"context"
	"fmt"
	"strings"
)

const DetectionInputVersion = "crosslink.detection.v1"

// DetectionSegment 只描述协议事实；Role、SourceType 和 ToolCallID 均不授予信任。
// Start/End 是 Content 中的 UTF-8 字节偏移，MessageIndex=-1 表示无消息位置。
type DetectionSegment struct {
	ID           string `json:"id"`
	Role         string `json:"role,omitempty"`
	SourceType   string `json:"source_type"`
	MessageIndex int    `json:"message_index"`
	ToolCallID   string `json:"tool_call_id,omitempty"`
	Path         string `json:"path,omitempty"`
	Text         string `json:"text"`
	Start        int    `json:"start"`
	End          int    `json:"end"`
}

// DetectionInput 是教师、学生与在线检测共用的正文视图。
// ContextAvailable 只允许服务端验证任务/策略后设置；当前抽取器始终为 false。
// Coverage 表示抽取/裁剪覆盖；模型还须单独报告自身窗口预算的覆盖。
type DetectionInput struct {
	Version          string             `json:"version"`
	Content          string             `json:"content"`
	Segments         []DetectionSegment `json:"segments"`
	ContextAvailable bool               `json:"context_available"`
	Coverage         string             `json:"coverage"`
}

func PlainDetectionInput(content string) *DetectionInput {
	input := &DetectionInput{Version: DetectionInputVersion, Content: content, Coverage: "full"}
	if content != "" {
		input.Segments = []DetectionSegment{{ID: "s0", SourceType: "unknown", MessageIndex: -1, Text: content, End: len(content)}}
	}
	return input
}

// Validate 阻止错位正文、重复 ID 或伪造完整覆盖流入结构化引擎。
func (in *DetectionInput) Validate() error {
	if in.Version != DetectionInputVersion || (in.Coverage != "full" && in.Coverage != "partial") {
		return fmt.Errorf("invalid detection input version or coverage")
	}
	if in.ContextAvailable {
		return fmt.Errorf("verified task context is not supported by detection v1")
	}
	seen := make(map[string]bool, len(in.Segments))
	end := 0
	for i, seg := range in.Segments {
		start := end
		if i > 0 {
			start++ // 抽取器以单个换行分隔字段。
		}
		if seg.ID == "" || seen[seg.ID] || seg.SourceType == "" || seg.MessageIndex < -1 || seg.Start != start || seg.End <= seg.Start || seg.End > len(in.Content) {
			return fmt.Errorf("invalid detection segment %d", i)
		}
		if i > 0 && in.Content[end:seg.Start] != "\n" || in.Content[seg.Start:seg.End] != seg.Text {
			return fmt.Errorf("detection segment %d does not match content", i)
		}
		seen[seg.ID] = true
		end = seg.End
	}
	// 截断恰好落在分隔符之后时允许末尾换行。
	if end != len(in.Content) && !(in.Coverage == "partial" && in.Content[end:] == "\n") {
		return fmt.Errorf("detection segments do not cover content")
	}
	return nil
}

// StructuredGuardrailEngine 为可选能力，旧引擎继续接收相同正文。
type StructuredGuardrailEngine interface {
	CheckInput(context.Context, *DetectionInput, Direction, string) (*GuardrailResult, error)
}

func checkEngine(ctx context.Context, eng GuardrailEngine, req *CheckRequest) (*GuardrailResult, error) {
	structured, ok := eng.(StructuredGuardrailEngine)
	if !ok {
		return eng.Check(ctx, req.Content, req.Direction, req.Model)
	}
	input := req.Input
	if input == nil {
		input = PlainDetectionInput(req.Content)
	}
	if err := input.Validate(); err != nil {
		return nil, err
	}
	// 每个引擎独占片段副本；服务级截断不得保留窗口外的正文或完整覆盖声明。
	view := *input
	view.Segments = append([]DetectionSegment(nil), input.Segments...)
	if input.Content != req.Content {
		if !strings.HasPrefix(input.Content, req.Content) {
			return nil, fmt.Errorf("detection input differs from checked content")
		}
		view.Content, view.Coverage = req.Content, "partial"
		view.Segments = nil
		for _, seg := range input.Segments {
			if seg.Start >= len(req.Content) {
				break
			}
			if seg.End > len(req.Content) {
				seg.End = len(req.Content)
				seg.Text = req.Content[seg.Start:seg.End]
			}
			view.Segments = append(view.Segments, seg)
		}
	}
	return structured.CheckInput(ctx, &view, req.Direction, req.Model)
}
