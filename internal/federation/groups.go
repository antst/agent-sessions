package federation

const (
	// SessionKindInteractive identifies a native interactive peer attachment.
	SessionKindInteractive = "interactive"
	// SessionKindLane identifies a daemon-owned durable lane attachment.
	SessionKindLane = "lane"
)

// SessionPreferences is the small durable portion of one peer registration.
type SessionPreferences struct {
	SessionID           string               `json:"session_id"`
	Product             string               `json:"product"`
	Kind                string               `json:"kind,omitempty"`
	ExplicitGroups      []string             `json:"explicit_groups,omitempty"`
	InheritedGroups     []string             `json:"inherited_groups,omitempty"`
	ParentSession       string               `json:"parent_session_id,omitempty"`
	ParentHostID        string               `json:"parent_host_id,omitempty"`
	InheritParentGroups bool                 `json:"inherit_parent_groups"`
	AlwaysApprove       bool                 `json:"always_approve"`
	Qwen                *QwenSessionMetadata `json:"qwen,omitempty"`
	UpdatedAt           int64                `json:"updated_at"`
	Revision            string               `json:"revision,omitempty"`
}

// QwenProfileIdentity is non-secret exact Qwen profile selection metadata.
type QwenProfileIdentity struct {
	QwenHomeSet    bool   `json:"qwen_home_set"`
	QwenHome       string `json:"qwen_home,omitempty"`
	QwenRuntimeSet bool   `json:"qwen_runtime_dir_set"`
	QwenRuntimeDir string `json:"qwen_runtime_dir,omitempty"`
	Fingerprint    string `json:"profile_fingerprint"`
}

// QwenSessionMetadata carries Qwen-specific non-secret launch preferences.
type QwenSessionMetadata struct {
	Cwd                string              `json:"cwd"`
	Profile            QwenProfileIdentity `json:"profile"`
	LaunchPreference   string              `json:"launch_permission_preference"`
	InitialModeRequest string              `json:"initial_mode_request,omitempty"`
}

// SessionPreferenceUpdate applies presence-sensitive group and mode changes.
type SessionPreferenceUpdate struct {
	SessionID              string
	Product                string
	Kind                   string
	ExplicitGroups         []string
	GroupsSpecified        bool
	ParentSession          string
	ParentHostID           string
	ParentGroups           []string
	ParentSpecified        bool
	InheritParentGroups    bool
	InheritGroupsSpecified bool
	AlwaysApprove          bool
	AlwaysApproveSpecified bool
	Qwen                   *QwenSessionMetadata
}
