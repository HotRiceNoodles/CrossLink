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
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Resolution      string  `json:"resolution,omitempty"`
	Cost            float64 `json:"cost,omitempty"`
}

// VideoCreateRequest — 公开 API 请求体 (POST /v1/videos)
type VideoCreateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt" binding:"required"`
	Seconds int   `json:"seconds,omitempty"`
	Size    string `json:"size,omitempty"`
	ImageReference *struct {
		ImageURL string `json:"image_url,omitempty"`
		FileID   string `json:"file_id,omitempty"`
	} `json:"image_reference,omitempty"`
}

// VideoResponse — 公开 API 响应 (OpenAI Videos 格式)
type VideoResponse struct {
	ID          string        `json:"id"`
	Object      string        `json:"object"`                   // "video"
	Status      string        `json:"status"`                   // queued/in_progress/completed/failed
	Model       string        `json:"model"`
	Progress    float64       `json:"progress,omitempty"`
	CreatedAt   int64         `json:"created_at"`
	CompletedAt *int64        `json:"completed_at,omitempty"`
	ExpiresAt   *int64        `json:"expires_at,omitempty"`
	Seconds     string        `json:"seconds,omitempty"`        // string per OpenAI spec
	Size        string        `json:"size,omitempty"`
	Output      []VideoOutput `json:"output,omitempty"`
	Error       *VideoError   `json:"error,omitempty"`
}

// VideoOutput — 完成后的下载入口
type VideoOutput struct {
	Type string `json:"type"` // "url"
	URL  string `json:"url"`  // gateway-relative "/v1/videos/{id}/content"
}

// VideoError — 任务失败时的错误信息
type VideoError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}
