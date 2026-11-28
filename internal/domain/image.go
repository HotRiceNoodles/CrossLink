package domain

type ImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt" binding:"required"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Style          string `json:"style,omitempty"`
	User           string `json:"user,omitempty"`
}

type ImageResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
}

type ImageData struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ImageEditRequest is the multipart /v1/images/edits payload (图片方案 P2,
// design §4.1). Deliberately NOT JSON-tagged: it arrives as multipart/form-data
// (the handler parses file parts and form fields) and leaves as a rebuilt
// multipart upstream (the adapter re-serializes) — it never rides a JSON wire.
type ImageEditRequest struct {
	Image          []byte // the source image bytes (required)
	ImageFilename  string // original filename, forwarded as the part filename
	Mask           []byte // optional transparency mask
	MaskFilename   string
	Prompt         string
	Model          string
	N              int
	Size           string
	Quality        string
	ResponseFormat string
	User           string
}
