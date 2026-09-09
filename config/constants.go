package config

// Canonical generation task statuses.
const (
	STATUS_SUCCESS   = "succeeded"
	STATUS_FAILED    = "failed"
	STATUS_RUNNING   = "running"
	STATUS_CANCELLED = "cancelled"
)

// Modalities — every model in the code catalog belongs to exactly one.
const (
	ModalityVideo = "video"
	ModalityAudio = "audio"
	ModalityImage = "image"
	ModalityText  = "text"
)
