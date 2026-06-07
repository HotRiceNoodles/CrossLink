package domain

// VideoRequest — 视频生成请求
type VideoRequest struct {
	Prompt         string `json:"prompt"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`    // "16:9", "9:16", "1:1"
	Duration       int    `json:"duration,omitempty"`        // 秒数，如 5, 10, 15
	ReferenceImage string `json:"reference_image,omitempty"` // base64 或 URL
	Model          string `json:"model,omitempty"`
}

// VideoTask — 异步视频生成任务实体
type VideoTask struct {
	TaskID       string      `json:"task_id"`
	Status       string      `json:"status"` // "pending", "processing", "completed", "failed"
	VideoURL     string      `json:"video_url,omitempty"`
	ThumbnailURL string      `json:"thumbnail_url,omitempty"`
	Error        string      `json:"error,omitempty"`
	Usage        *VideoUsage `json:"usage,omitempty"`
	Model        string      `json:"model,omitempty"`
	ProviderName string      `json:"provider_name,omitempty"`
}

// VideoUsage — 视频生成用量
type VideoUsage struct {
	DurationSeconds int     `json:"duration_seconds,omitempty"`
	Resolution      string  `json:"resolution,omitempty"`
	Cost            float64 `json:"cost,omitempty"`
}
