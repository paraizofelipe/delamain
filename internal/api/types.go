package api

// Repo is a repository the API may start or queue.
type Repo struct {
	Path  string    `json:"path"`
	Name  string    `json:"name"`
	Forge *ForgeRef `json:"forge,omitempty"`
}

// ForgeRef is where a repository lives on its forge.
type ForgeRef struct {
	Kind string `json:"kind"` // github | gitlab
	Host string `json:"host"`
	Path string `json:"path"` // owner/repo
}
