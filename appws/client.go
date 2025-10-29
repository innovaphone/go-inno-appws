package appws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	mtAppChallenge     = "AppChallenge"
	mtAppChallengeResp = "AppChallengeResult"
	mtAppLogin         = "AppLogin"
	mtAppLoginResp     = "AppLoginResult"
	mtKeepAlive        = "KeepAlive"
)

// Client represents an active AppWebsocket connection.
type Client struct {
	conn *websocket.Conn
	cfg  Config

	defaultTimeout time.Duration
	pingInterval   time.Duration

	sendMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[*pendingRequest]struct{}

	subsMu sync.RWMutex
	subs   map[subKey]*subscription

	readDone chan struct{}
	closed   chan struct{}

	failOnce sync.Once
	closeErr error

	stopKeepAlive context.CancelFunc

	sessionKey string
}

type pendingRequest struct {
	api       string
	requestMT string
	src       *string
	ch        chan pendingResult
}

type pendingResult struct {
	data []byte
	err  error
}

type subKey struct {
	api string
	src string
}

type subscription struct {
	events chan []byte
}

// Dial performs the AppChallenge/AppLogin handshake and returns a ready client.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("appws: URL must be provided")
	}
	if cfg.App == "" {
		return nil, fmt.Errorf("appws: App must be provided")
	}
	if cfg.Password == "" {
		return nil, fmt.Errorf("appws: Password must be provided")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	dialCtx, cancelDial := withMaybeTimeout(ctx, timeout)
	defer cancelDial()

	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("appws: dial: %w", err)
	}

	sessionKey, backlog, err := performHandshake(dialCtx, conn, cfg, timeout)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	client := &Client{
		conn:           conn,
		cfg:            cfg,
		defaultTimeout: timeout,
		pingInterval:   cfg.PingInterval,
		pending:        make(map[*pendingRequest]struct{}),
		subs:           make(map[subKey]*subscription),
		readDone:       make(chan struct{}),
		closed:         make(chan struct{}),
		sessionKey:     sessionKey,
	}

	go client.readLoop(backlog)
	if cfg.PingInterval > 0 {
		client.startKeepAlive()
	}

	return client, nil
}

// Close gracefully closes the connection.
func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}

	deadline := time.Now().Add(3 * time.Second)
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
	_ = c.conn.Close()
	c.fail(ErrClientClosed)

	if ctx == nil {
		ctx = context.Background()
	}

	select {
	case <-c.readDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendOption configures optional parameters for Send/Do.
type SendOption func(*sendOptions)

type sendOptions struct{ src *string }

// WithSrc includes the provided src in the outgoing message.
func WithSrc(src string) SendOption { return func(o *sendOptions) { o.src = &src } }

// Do sends a request and waits for the first matching *Result.
// If no WithSrc is provided, no src is sent; callers must serialize overlapping
// Do calls for the same api/mt when omitting src.
func (c *Client) Do(ctx context.Context, api, mt string, payload any, result any, opts ...SendOption) error {
	if c == nil {
		return fmt.Errorf("appws: client is nil")
	}
	if mt == "" {
		return fmt.Errorf("appws: mt must be provided")
	}
	if c.isClosed() {
		return ErrClientClosed
	}

	data, src, err := buildEnvelope(api, mt, payload, opts)
	if err != nil {
		return err
	}

	pr := &pendingRequest{
		api:       api,
		requestMT: mt,
		src:       src,
		ch:        make(chan pendingResult, 1),
	}

	c.pendingMu.Lock()
	c.pending[pr] = struct{}{}
	c.pendingMu.Unlock()

	writeCtx, cancel := withMaybeTimeout(ctx, c.defaultTimeout)
	defer cancel()

	if err := c.write(writeCtx, data); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, pr)
		c.pendingMu.Unlock()
		return err
	}

	if ctx == nil {
		ctx = context.Background()
	}

	select {
	case res := <-pr.ch:
		if res.err != nil {
			return res.err
		}
		if result != nil {
			if err := json.Unmarshal(res.data, result); err != nil {
				return fmt.Errorf("appws: decode result: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		if _, ok := c.pending[pr]; ok {
			delete(c.pending, pr)
		}
		c.pendingMu.Unlock()
		return ctx.Err()
	case <-c.closed:
		return c.closeErr
	}
}

// Send is fire-and-forget. Same src rule as Do.
func (c *Client) Send(ctx context.Context, api, mt string, payload any, opts ...SendOption) error {
	if c == nil {
		return fmt.Errorf("appws: client is nil")
	}
	if mt == "" {
		return fmt.Errorf("appws: mt must be provided")
	}
	if c.isClosed() {
		return ErrClientClosed
	}

	data, _, err := buildEnvelope(api, mt, payload, opts)
	if err != nil {
		return err
	}

	writeCtx, cancel := withMaybeTimeout(ctx, c.defaultTimeout)
	defer cancel()

	return c.write(writeCtx, data)
}

// Subscription is a stream of Info events for a given caller-provided src.
type Subscription struct {
	Events <-chan []byte
	Stop   func() error
}

// Subscribe requires a non-empty src from the caller; the library never generates it.
func (c *Client) Subscribe(ctx context.Context, api, mt, src string, payload any) (*Subscription, error) {
	if c == nil {
		return nil, fmt.Errorf("appws: client is nil")
	}
	if src == "" {
		return nil, fmt.Errorf("appws: src must be provided")
	}
	if mt == "" {
		return nil, fmt.Errorf("appws: mt must be provided")
	}
	if c.isClosed() {
		return nil, ErrClientClosed
	}

	data, _, err := buildEnvelope(api, mt, payload, []SendOption{WithSrc(src)})
	if err != nil {
		return nil, err
	}

	key := subKey{api: api, src: src}
	sub := &subscription{events: make(chan []byte, 16)}

	c.subsMu.Lock()
	if _, exists := c.subs[key]; exists {
		c.subsMu.Unlock()
		return nil, fmt.Errorf("appws: subscription for api=%q src=%q already exists", api, src)
	}
	c.subs[key] = sub
	c.subsMu.Unlock()

	writeCtx, cancel := withMaybeTimeout(ctx, c.defaultTimeout)
	defer cancel()

	if err := c.write(writeCtx, data); err != nil {
		c.subsMu.Lock()
		delete(c.subs, key)
		c.subsMu.Unlock()
		close(sub.events)
		return nil, err
	}

	stop := func() error {
		c.closeSubscription(key)
		return nil
	}

	return &Subscription{Events: sub.events, Stop: stop}, nil
}

func (c *Client) write(ctx context.Context, data []byte) error {
	deadline := deadlineFromContext(ctx, c.defaultTimeout)
	if !deadline.IsZero() {
		_ = c.conn.SetWriteDeadline(deadline)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.fail(err)
		return fmt.Errorf("appws: write: %w", err)
	}
	return nil
}

func (c *Client) readLoop(backlog [][]byte) {
	defer close(c.readDone)

	for _, raw := range backlog {
		c.handleMessage(raw)
	}

	for {
		if c.isClosed() {
			return
		}
		if c.pingInterval > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.pingInterval * 2))
		} else {
			_ = c.conn.SetReadDeadline(time.Time{})
		}
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		c.handleMessage(data)
	}
}

func (c *Client) handleMessage(raw []byte) {
	var msg baseMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	switch msg.MT {
	case mtKeepAlive, mtAppChallengeResp, mtAppLoginResp:
		return
	}

	if c.dispatchPending(msg, raw) {
		return
	}
	if msg.SRC != "" {
		_ = c.dispatchSubscription(msg, raw)
	}
}

func (c *Client) dispatchPending(msg baseMsg, raw []byte) bool {
	c.pendingMu.Lock()
	var target *pendingRequest
	for pr := range c.pending {
		if pr.matches(msg) {
			target = pr
			delete(c.pending, pr)
			break
		}
	}
	c.pendingMu.Unlock()

	if target == nil {
		return false
	}

	copyBytes := append([]byte(nil), raw...)
	target.deliver(pendingResult{data: copyBytes})
	return true
}

func (c *Client) dispatchSubscription(msg baseMsg, raw []byte) bool {
	key := subKey{api: msg.API, src: msg.SRC}
	c.subsMu.RLock()
	sub := c.subs[key]
	c.subsMu.RUnlock()
	if sub == nil {
		return false
	}

	copyBytes := append([]byte(nil), raw...)
	select {
	case sub.events <- copyBytes:
	default:
	}
	return true
}

func (c *Client) closeSubscription(key subKey) {
	c.subsMu.Lock()
	sub := c.subs[key]
	if sub != nil {
		delete(c.subs, key)
	}
	c.subsMu.Unlock()
	if sub != nil {
		close(sub.events)
	}
}

func (c *Client) startKeepAlive() {
	ctx, cancel := context.WithCancel(context.Background())
	c.stopKeepAlive = cancel
	go func(interval time.Duration) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if c.isClosed() {
					return
				}
				_ = c.Send(context.Background(), "", mtKeepAlive, nil)
			case <-ctx.Done():
				return
			}
		}
	}(c.pingInterval)
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Client) fail(err error) {
	c.failOnce.Do(func() {
		c.closeErr = err
		if c.stopKeepAlive != nil {
			c.stopKeepAlive()
		}
		close(c.closed)
		_ = c.conn.Close()

		c.pendingMu.Lock()
		pending := make([]*pendingRequest, 0, len(c.pending))
		for pr := range c.pending {
			pending = append(pending, pr)
		}
		c.pending = make(map[*pendingRequest]struct{})
		c.pendingMu.Unlock()
		for _, pr := range pending {
			pr.deliver(pendingResult{err: err})
		}

		c.subsMu.Lock()
		subs := c.subs
		c.subs = make(map[subKey]*subscription)
		c.subsMu.Unlock()
		for _, sub := range subs {
			close(sub.events)
		}
	})
}

func (pr *pendingRequest) matches(msg baseMsg) bool {
	if pr.src != nil {
		if msg.SRC == *pr.src {
			if pr.api != "" && msg.API != "" && pr.api != msg.API {
				return false
			}
			return true
		}
		if msg.SRC != "" {
			return false
		}
		// fall back to mt-based match when server omits src in the reply
		if pr.api != "" && msg.API != "" && pr.api != msg.API {
			return false
		}
		return matchResultMT(pr.requestMT, msg.MT)
	}
	if pr.api != "" && msg.API != "" && pr.api != msg.API {
		return false
	}
	return matchResultMT(pr.requestMT, msg.MT)
}

func (pr *pendingRequest) deliver(res pendingResult) {
	select {
	case pr.ch <- res:
	default:
	}
}

func performHandshake(ctx context.Context, conn *websocket.Conn, cfg Config, timeout time.Duration) (string, [][]byte, error) {
	challengeMsg := map[string]any{
		"mt":     mtAppChallenge,
		"appObj": cfg.App,
	}
	if cfg.SIP != "" {
		challengeMsg["user"] = cfg.SIP
	}
	if err := writeJSONWithTimeout(ctx, conn, timeout, challengeMsg); err != nil {
		return "", nil, fmt.Errorf("appws: send AppChallenge: %w", err)
	}

	backlog := make([][]byte, 0)
	var challenge string

	for challenge == "" {
		base, raw, err := readMessageWithTimeout(ctx, conn, timeout)
		if err != nil {
			return "", nil, fmt.Errorf("appws: read AppChallengeResult: %w", err)
		}
		if base.MT != mtAppChallengeResp {
			backlog = append(backlog, raw)
			continue
		}
		var res struct {
			baseMsg
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return "", nil, fmt.Errorf("appws: decode AppChallengeResult: %w", err)
		}
		if res.Challenge == "" {
			return "", nil, fmt.Errorf("appws: empty challenge received")
		}
		challenge = res.Challenge
	}

	loginMsg, sessionKey, err := buildLoginMessage(cfg, challenge)
	if err != nil {
		return "", nil, err
	}
	if err := writeJSONWithTimeout(ctx, conn, timeout, loginMsg); err != nil {
		return "", nil, fmt.Errorf("appws: send AppLogin: %w", err)
	}

	for {
		base, raw, err := readMessageWithTimeout(ctx, conn, timeout)
		if err != nil {
			return "", nil, fmt.Errorf("appws: read AppLoginResult: %w", err)
		}
		if base.MT == mtAppLoginResp {
			var res struct {
				baseMsg
				OK        bool   `json:"ok"`
				Error     string `json:"error,omitempty"`
				ErrorText string `json:"errorText,omitempty"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return "", nil, fmt.Errorf("appws: decode AppLoginResult: %w", err)
			}
			if !res.OK {
				reason := res.ErrorText
				if reason == "" {
					reason = res.Error
				}
				if reason == "" {
					reason = "login rejected"
				}
				return "", nil, fmt.Errorf("appws: login failed: %s", reason)
			}
			return sessionKey, backlog, nil
		}
		backlog = append(backlog, raw)
	}
}

func buildLoginMessage(cfg Config, challenge string) (map[string]any, string, error) {
	infoString := ""
	if cfg.Info != nil {
		b, err := marshalNoEscape(cfg.Info)
		if err != nil {
			return nil, "", fmt.Errorf("appws: marshal info: %w", err)
		}
		infoString = string(b)
	}

	parts := []string{cfg.App, cfg.Domain, cfg.SIP, cfg.GUID, cfg.DN}
	if infoString != "" {
		parts = append(parts, infoString)
	}
	parts = append(parts, challenge, cfg.Password)
	sum := sha256.Sum256([]byte(strings.Join(parts, ":")))
	digest := hex.EncodeToString(sum[:])

	keySource := "innovaphoneAppSessionKey:" + challenge + ":" + cfg.Password
	sessionKey := sha256Hex(keySource)

	login := map[string]any{
		"mt":     mtAppLogin,
		"app":    cfg.App,
		"domain": cfg.Domain,
		"sip":    cfg.SIP,
		"guid":   cfg.GUID,
		"dn":     cfg.DN,
		"digest": digest,
		"key":    sessionKey,
		"pbxObj": cfg.App,
	}
	if cfg.Info != nil {
		login["info"] = cfg.Info
	}
	return login, sessionKey, nil
}

func sha256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

func buildEnvelope(api, mt string, payload any, opts []SendOption) ([]byte, *string, error) {
	var options sendOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}

	envelope := map[string]any{
		"mt": mt,
	}
	if api != "" {
		envelope["api"] = api
	}
	var srcPtr *string
	if options.src != nil {
		s := *options.src
		envelope["src"] = s
		srcPtr = &s
	}

	if payload != nil {
		if err := mergePayload(envelope, payload); err != nil {
			return nil, nil, err
		}
	}

	data, err := marshalNoEscape(envelope)
	if err != nil {
		return nil, nil, fmt.Errorf("appws: marshal message: %w", err)
	}
	return data, srcPtr, nil
}

func mergePayload(target map[string]any, payload any) error {
	switch p := payload.(type) {
	case map[string]any:
		for k, v := range p {
			if k == "mt" || k == "api" || k == "src" {
				return fmt.Errorf("appws: payload field %q conflicts with reserved envelope field", k)
			}
			target[k] = v
		}
		return nil
	case json.RawMessage:
		if len(p) == 0 {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(p, &m); err != nil {
			return err
		}
		return mergePayload(target, m)
	case []byte:
		if len(p) == 0 {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(p, &m); err != nil {
			return err
		}
		return mergePayload(target, m)
	default:
		b, err := marshalNoEscape(p)
		if err != nil {
			return err
		}
		if len(b) == 0 || bytes.Equal(b, []byte("null")) {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		return mergePayload(target, m)
	}
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func writeJSONWithTimeout(ctx context.Context, conn *websocket.Conn, fallback time.Duration, v any) error {
	data, err := marshalNoEscape(v)
	if err != nil {
		return err
	}
	deadline := deadlineFromContext(ctx, fallback)
	if !deadline.IsZero() {
		_ = conn.SetWriteDeadline(deadline)
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

func readMessageWithTimeout(ctx context.Context, conn *websocket.Conn, fallback time.Duration) (baseMsg, []byte, error) {
	deadline := deadlineFromContext(ctx, fallback)
	if !deadline.IsZero() {
		_ = conn.SetReadDeadline(deadline)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		return baseMsg{}, nil, err
	}
	var msg baseMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return baseMsg{}, nil, err
	}
	return msg, data, nil
}

func deadlineFromContext(ctx context.Context, fallback time.Duration) time.Time {
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			return deadline
		}
	}
	if fallback <= 0 {
		return time.Time{}
	}
	return time.Now().Add(fallback)
}

func withMaybeTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if d <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= d {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func matchResultMT(request, response string) bool {
	if response == request {
		return true
	}
	if response == request+"Result" {
		return true
	}
	if strings.HasSuffix(request, "Request") {
		base := strings.TrimSuffix(request, "Request")
		if response == base+"Result" {
			return true
		}
	}
	if strings.HasSuffix(request, "Req") {
		base := strings.TrimSuffix(request, "Req")
		if response == base+"Resp" || response == base+"Result" {
			return true
		}
	}
	return false
}
