package domain

type BatchRequest struct {
	InputFileID      string `json:"input_file_id" binding:"required"`
	Endpoint         string `json:"endpoint" binding:"required"`
	CompletionWindow string `json:"completion_window"`
	Metadata         any    `json:"metadata,omitempty"`
}

type BatchResponse struct {
	ID               string              `json:"id"`
	Object           string              `json:"object"`
	Endpoint         string              `json:"endpoint"`
	InputFileID      string              `json:"input_file_id"`
	CompletionWindow string              `json:"completion_window"`
	Status           string              `json:"status"`
	OutputFileID     string              `json:"output_file_id,omitempty"`
	ErrorFileID      string              `json:"error_file_id,omitempty"`
	CreatedAt        int64               `json:"created_at"`
	ExpiresAt        int64               `json:"expires_at,omitempty"`
	RequestCounts    *BatchRequestCounts `json:"request_counts,omitempty"`
	Metadata         any                 `json:"metadata,omitempty"`
}

type BatchRequestCounts struct {
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Total     int `json:"total"`
}

type BatchListResponse struct {
	Data    []BatchResponse `json:"data"`
	Object  string          `json:"object"`
	First   string          `json:"first_id,omitempty"`
	Last    string          `json:"last_id,omitempty"`
	HasMore bool            `json:"has_more"`
}
