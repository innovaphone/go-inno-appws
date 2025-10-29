package appws

import "time"

// Config contains connection, handshake and timing parameters.
type Config struct {
	URL          string        // WebSocket endpoint (wss://...)
	App          string        // App object name
	Password     string        // Shared secret
	Domain       string        // optional
	SIP          string        // optional
	GUID         string        // optional
	DN           string        // optional
	Info         any           // optional (minified in digest when present)
	Timeout      time.Duration // per-operation default timeout
	PingInterval time.Duration // keepalive interval
}
