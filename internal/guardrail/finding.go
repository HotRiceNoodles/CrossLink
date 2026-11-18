package guardrail

// Finding 与 Precision：检测引擎的定位证据契约（M0 冻结基线 v1.3 §2.1/C1，
// 规划 §4「zone 感知底座」）。本文件只定义共享 DTO——引擎产出 Finding、策略
// 层消费 Finding 的装配属商业侧 kb_poisoning（M1）；社区构建引入这些类型但
// 无行为变化。
//
// 坐标纪律（冻结件 §2.1，三层文本空间）：
//
//	协议 JSON 字段 ──解码──▶ 原始文本段（segment）──规范化/解码──▶ 检测文本
//
// Finding.RawSpans 一律为**原文坐标**（ExtractedContent.Text 字节空间，与
// MaskedSpans 同一坐标系——applyRequestMask 已依赖该不变量）。引擎在检测
// 文本上的命中须经 transform 逆映射回原文；映射失败降级 Precision，禁止把
// 检测文本偏移当原文偏移使用。

// Precision 表示一条 Finding 定位证据的精度（冻结件 C1）。
type Precision string

const (
	// PrecisionExact：raw_spans 可精确定位原文（可作 span 级 sanitize 依据）。
	PrecisionExact Precision = "exact"
	// PrecisionSegmentOnly：只知所属协议段（解码命中/指纹整段命中等）——
	// 仅可段级处置，不得伪造精确删除 span。
	PrecisionSegmentOnly Precision = "segment_only"
	// PrecisionUnknown：无法定位（含解码超限）——处置按覆盖矩阵，不作为
	// sanitize 依据。
	PrecisionUnknown Precision = "unknown"
)

// Finding 是一条带定位证据的检测结果。Score 是风险分数（zw-v2 聚合输入），
// 不是概率；Transform 记录原文→检测文本的变换链标识（空 = 未变换）。
type Finding struct {
	PatternID string
	Category  string
	RawSpans  []Span
	Score     float64
	Transform string
	Precision Precision
}

// Zones 把 Finding 的定位证据映射到协议 zone 集合（span→segment→zone，
// 精确、零拷贝；见 ExtractedContent.SpanZones）。ex 为 nil 时返回 nil。
func (f Finding) Zones(ex *ExtractedContent) []string {
	if ex == nil || len(f.RawSpans) == 0 {
		return nil
	}
	return ex.SpanZones(f.RawSpans)
}
