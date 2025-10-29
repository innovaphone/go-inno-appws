package appws

// baseMsg is the minimal envelope for AppWebsocket messages.
type baseMsg struct {
	MT  string `json:"mt"`
	API string `json:"api,omitempty"`
	SRC string `json:"src,omitempty"`
}
