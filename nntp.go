package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nntpClient is a minimal NNTP (news) client: connect, auth, BODY/ARTICLE.
type nntpClient struct {
	cfg  NNTPServer
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	mu   sync.Mutex
}

func dialNNTP(ctx context.Context, cfg NNTPServer) (*nntpClient, error) {
	var d net.Dialer
	d.Timeout = 20 * time.Second
	var conn net.Conn
	var err error
	if cfg.TLS {
		tconn, derr := tls.DialWithDialer(&d, "tcp", cfg.Addr(), &tls.Config{
			ServerName: cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
		conn, err = tconn, derr
	} else {
		conn, err = d.DialContext(ctx, "tcp", cfg.Addr())
	}
	if err != nil {
		return nil, err
	}
	c := &nntpClient{
		cfg:  cfg,
		conn: conn,
		r:    bufio.NewReaderSize(conn, 256*1024),
		w:    bufio.NewWriterSize(conn, 64*1024),
	}
	code, greetText, err := c.readStatus()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	if code != 200 {
		conn.Close()
		if connLimitText(greetText) {
			return nil, fmt.Errorf("%w: %s", errConnLimit, greetText)
		}
		return nil, fmt.Errorf("nntp greeting code %d", code)
	}
	// MODE READER enables BODY; harmless if unsupported.
	_, _, _ = c.command("MODE READER")
	if cfg.Username != "" {
		if err := c.auth(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("auth: %w", err)
		}
	}
	return c, nil
}

func (c *nntpClient) auth() error {
	code, text, err := c.command("AUTHINFO USER %s", c.cfg.Username)
	if err != nil {
		return err
	}
	if code == 480 || code == 502 {
		if connLimitText(text) {
			return fmt.Errorf("%w: %s", errConnLimit, text)
		}
		return errors.New("server rejected auth user")
	}
	if code != 381 && code != 281 {
		// Some servers accept PASS directly.
	}
	code, text, err = c.command("AUTHINFO PASS %s", c.cfg.Password)
	if err != nil {
		return err
	}
	if code != 281 && code != 503 {
		// 503 = auth accepted but not needed / transient
		if code >= 400 {
			if connLimitText(text) {
				return fmt.Errorf("%w: %s", errConnLimit, text)
			}
			return fmt.Errorf("auth failed code %d", code)
		}
	}
	return nil
}

// errConnLimit marks provider capacity refusals ("502 connection limit
// reached", "too many connections"). Distinct from bad credentials: it is
// transient and worth backing off + retrying rather than failing a fetch.
var errConnLimit = errors.New("nntp connection limit reached")

func connLimitText(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "limit") || strings.Contains(t, "too many")
}

func (c *nntpClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.Close()
}

// command sends a single-line command and reads the status line.
func (c *nntpClient) command(format string, args ...any) (code int, text string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetDeadline(time.Now().Add(60 * time.Second))
	if err = c.writeCmd(format, args...); err != nil {
		return 0, "", err
	}
	line, err := c.readLine()
	if err != nil {
		return 0, "", err
	}
	code, err = parseStatus(line)
	if err != nil {
		return 0, line, err
	}
	return code, line, nil
}

func parseStatus(line string) (int, error) {
	line = strings.TrimSpace(line)
	if len(line) < 3 {
		return 0, fmt.Errorf("short status %q", line)
	}
	return strconv.Atoi(line[:3])
}

func (c *nntpClient) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *nntpClient) readStatus() (int, string, error) {
	line, err := c.readLine()
	if err != nil {
		return 0, "", err
	}
	code, err := parseStatus(line)
	return code, line, err
}

// writeCmd formats one NNTP command line with all control characters
// stripped — a malicious message-id or credential can otherwise inject
// arbitrary commands into the session.
func (c *nntpClient) writeCmd(format string, args ...any) error {
	line := fmt.Sprintf(format, args...)
	line = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, line)
	if _, err := fmt.Fprintf(c.w, "%s\r\n", line); err != nil {
		return err
	}
	return c.w.Flush()
}

var errNoSuchArticle = errors.New("no such article")

// fetchBody retrieves an article body by message-id using BODY, falling back
// to ARTICLE (headers stripped). Returns decoded NNTP payload (dot-unstuffed).
func (c *nntpClient) fetchBody(msgID string) ([]byte, error) {
	// Servers require the <id@host> form; NZBs commonly ship it bare, and a
	// bare id is read as an article number (412 No newsgroup selected).
	if !strings.HasPrefix(msgID, "<") {
		msgID = "<" + msgID + ">"
	}
	code, payload, err := c.commandMulti("BODY %s", msgID)
	if err == nil {
		switch code {
		case 220, 221, 222: // 222 = "body follows" per RFC 3977
			return payload, nil
		case 430:
			return nil, errNoSuchArticle
		case 480:
			return nil, errors.New("authentication required")
		}
		// 5xx or unexpected → try ARTICLE below for non-220 codes too
	}
	// Fallback: ARTICLE includes headers.
	code, payload, err = c.commandMulti("ARTICLE %s", msgID)
	if err != nil {
		return nil, err
	}
	switch code {
	case 220, 221:
		// Strip headers: everything up to first blank line.
		if i := bytes_indexDoubleNewline(payload); i >= 0 {
			return payload[i:], nil
		}
		return payload, nil
	case 430:
		return nil, errNoSuchArticle
	case 480:
		return nil, errors.New("authentication required")
	default:
		return nil, fmt.Errorf("article %s: code %d", msgID, code)
	}
}

// commandMulti sends a command expecting a multi-line response terminated by
// a lone "." line (NNTP dot-stuffing undone). Returns status code + payload.
func (c *nntpClient) commandMulti(format string, args ...any) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetDeadline(time.Now().Add(120 * time.Second))
	if err := c.writeCmd(format, args...); err != nil {
		return 0, nil, err
	}
	line, err := c.readLine()
	if err != nil {
		return 0, nil, err
	}
	code, err := parseStatus(line)
	if err != nil {
		return 0, nil, fmt.Errorf("bad status %q: %w", line, err)
	}
	if code < 200 || code >= 300 {
		return code, nil, nil
	}
	var sb strings.Builder
	sb.Grow(1 << 20)
	const maxArticle = 64 << 20
	for {
		l, err := c.readLine()
		if err != nil {
			return 0, nil, err
		}
		if l == "." {
			break
		}
		// Dot-unstuff: "..x" -> ".x"
		if strings.HasPrefix(l, "..") {
			l = l[1:]
		}
		sb.WriteString(l)
		sb.WriteByte('\n')
		if sb.Len() > maxArticle {
			return 0, nil, errors.New("article too large")
		}
	}
	return code, []byte(sb.String()), nil
}

func bytes_indexDoubleNewline(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i + 2
		}
		if i+3 < len(b) && b[i] == '\r' && b[i+1] == '\n' && b[i+2] == '\r' && b[i+3] == '\n' {
			return i + 4
		}
	}
	return -1
}

// nntpPool hands out connections to a server, dialing on demand.
// `max` caps total live connections (idle + in-use) to the server.
type nntpPool struct {
	cfg     NNTPServer
	max     int
	mu      sync.Mutex
	idle    []*nntpClient
	active  int
	closed  bool
	waiters []chan struct{}
}

func newNNTPPool(cfg NNTPServer) *nntpPool {
	p := &nntpPool{cfg: cfg, max: cfg.Connections}
	if p.max <= 0 {
		p.max = 4
	}
	return p
}

func (p *nntpPool) signal() {
	// Wake all waiters; they re-check conditions. Scale is tiny.
	for _, ch := range p.waiters {
		close(ch)
	}
	p.waiters = nil
}

func (p *nntpPool) Acquire(ctx context.Context) (*nntpClient, error) {
	limRetries := 0
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool closed")
		}
		// Reuse an idle conn if any are healthy.
		for len(p.idle) > 0 {
			c := p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]
			if c.connOK() {
				p.mu.Unlock()
				return c, nil
			}
			p.active--
			c.Close()
		}
		if p.active < p.max {
			p.active++
			p.mu.Unlock()
			c, err := dialNNTP(ctx, p.cfg)
			if err != nil {
				p.mu.Lock()
				p.active--
				p.signal()
				p.mu.Unlock()
				// Provider at capacity: back off and retry instead of
				// burning the caller's fetch attempt. Bounded so a
				// permanently saturated account still surfaces an error.
				if errors.Is(err, errConnLimit) && limRetries < 10 {
					limRetries++
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(time.Duration(300*limRetries) * time.Millisecond):
					}
					continue
				}
				return nil, err
			}
			return c, nil
		}
		ch := make(chan struct{})
		p.waiters = append(p.waiters, ch)
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			// Leave the waiter registered; it will be closed on next signal
			// and the loop won't run again since we return.
			return nil, ctx.Err()
		}
	}
}

func (c *nntpClient) connOK() bool {
	// Cheap liveness check; real failures surface on fetch and evict the conn.
	return c.conn != nil
}

func (p *nntpPool) Release(c *nntpClient, bad bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if bad {
		p.active--
		c.Close()
	} else {
		p.idle = append(p.idle, c)
	}
	p.signal()
}

func (p *nntpPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, c := range p.idle {
		c.Close()
	}
	p.idle = nil
	p.signal()
}

var _ = io.EOF
