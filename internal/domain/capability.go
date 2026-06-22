package domain

// CapabilityModality is the endpoint family a capability serves.
type CapabilityModality string

const (
	ModalityText  CapabilityModality = "text"
	ModalityImage CapabilityModality = "image"
	ModalityAudio CapabilityModality = "audio"
	ModalityEmbed CapabilityModality = "embed"
)

// ValidModality reports whether m is a supported modality.
func ValidModality(m CapabilityModality) bool {
	switch m {
	case ModalityText, ModalityImage, ModalityAudio, ModalityEmbed:
		return true
	}
	return false
}
