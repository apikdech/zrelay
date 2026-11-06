package sidecar

// StatusSchema is the schema of status/relay.json (Contract §7.5.1).
const StatusSchema = "zrec.status/1"

// Status is the relay status document (Contract §7.5.1), plus the additive
// storage field (design §6.6).
type Status struct {
	Schema    string         `json:"schema"`
	RelayID   string         `json:"relay_id"`
	Version   string         `json:"version"`
	UpdatedAt Time           `json:"updated_at"`
	KeyID     string         `json:"key_id"`
	Cameras   []CameraStatus `json:"cameras"`
	Storage   *StorageStatus `json:"storage,omitempty"`
}

// CameraStatus is one entry of Status.Cameras.
type CameraStatus struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	RTSP       string `json:"rtsp"`   // up | down | degraded
	Events     string `json:"events"` // up | down | degraded
	Since      Time   `json:"since"`
	LastClipAt *Time  `json:"last_clip_at"` // null when no clip yet
}

// StorageStatus reports the egress circuit breaker (design §6.6).
type StorageStatus struct {
	State        string `json:"state"`
	Since        Time   `json:"since"`
	BacklogBytes int64  `json:"backlog_bytes"`
}
