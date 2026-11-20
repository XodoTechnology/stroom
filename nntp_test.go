package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeNNTP is a minimal NNTP server for tests.
type fakeNNTP struct {
	ln       net.Listener
	articles map[string]string // msgID -> body
	authUser string
	authPass string
	gotAuth  bool
}

func startFakeNNTP(t *testing.T, articles map[string]string, user, pass string) *fakeNNTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNNTP{ln: ln, articles: articles, authUser: user, authPass: pass}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeNNTP) addr() string { return f.ln.Addr().String() }

func (f *fakeNNTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeNNTP) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	fmt.Fprintln(w, "200 fake.nntp ready")
	w.Flush()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "MODE"):
			fmt.Fprintln(w, "200 mode ok")
		case strings.HasPrefix(upper, "AUTHINFO USER"):
			if f.authUser == "" {
				fmt.Fprintln(w, "502 no auth needed")
			} else {
				f.gotAuth = true
				fmt.Fprintln(w, "381 password required")
			}
		case strings.HasPrefix(upper, "AUTHINFO PASS"):
			parts := strings.Fields(line)
			if len(parts) >= 3 && parts[2] == f.authPass {
				fmt.Fprintln(w, "281 auth ok")
			} else {
				fmt.Fprintln(w, "480 auth failed")
			}
		case strings.HasPrefix(upper, "BODY "), strings.HasPrefix(upper, "ARTICLE "):
			id := strings.TrimSpace(line[strings.IndexByte(line, ' ')+1:])
			id = strings.TrimPrefix(strings.TrimSuffix(id, ">"), "<") // real servers accept <id>; map keys are bare
			body, ok := f.articles[id]
			if !ok {
				fmt.Fprintln(w, "430 no such article")
			} else {
				if strings.HasPrefix(upper, "ARTICLE") {
					fmt.Fprintln(w, "220 article follows")
					fmt.Fprintln(w, "Path: test")
					fmt.Fprintln(w, "")
				} else {
					fmt.Fprintln(w, "222 body follows")
				}
				// Dot-stuffed body.
				for _, l := range strings.Split(body, "\n") {
					if strings.HasPrefix(l, ".") {
						l = "." + l
					}
					fmt.Fprintln(w, l)
				}
				fmt.Fprintln(w, ".")
			}
		case upper == "QUIT":
			fmt.Fprintln(w, "205 bye")
			w.Flush()
			return
		default:
			fmt.Fprintln(w, "500 unknown")
		}
		w.Flush()
	}
}

func TestNNTPFetchBody(t *testing.T) {
	articles := map[string]string{
		"msg1@x": "line1\nline2\n.yenc-looking but not at col0",
		".dot@x": ".leading dot line\nnormal",
	}
	f := startFakeNNTP(t, articles, "", "")
	host, port, _ := net.SplitHostPort(f.addr())
	cfg := NNTPServer{Host: host, Port: mustAtoi(port)}

	c, err := dialNNTP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	body, err := c.fetchBody("msg1@x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "line1") {
		t.Fatalf("bad body: %q", body)
	}
	// Dot-unstuffing: ".leading" should lose one dot.
	body2, err := c.fetchBody(".dot@x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body2), ".leading dot line") {
		t.Fatalf("dot unstuff failed: %q", body2)
	}
	// Missing article.
	if _, err := c.fetchBody("nope@x"); err == nil {
		t.Fatal("expected error for missing article")
	}
}

func TestNNTPAuth(t *testing.T) {
	articles := map[string]string{"m@x": "data"}
	f := startFakeNNTP(t, articles, "user1", "pass1")
	host, port, _ := net.SplitHostPort(f.addr())
	cfg := NNTPServer{Host: host, Port: mustAtoi(port), Username: "user1", Password: "pass1"}

	c, err := dialNNTP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	body, err := c.fetchBody("m@x")
	if err != nil || strings.TrimSpace(string(body)) != "data" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestNNTPPool(t *testing.T) {
	articles := map[string]string{}
	f := startFakeNNTP(t, articles, "", "")
	host, port, _ := net.SplitHostPort(f.addr())
	cfg := NNTPServer{Host: host, Port: mustAtoi(port), Connections: 3}
	pool := newNNTPPool(cfg)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Acquire up to limit, release, reacquire.
	var cs []*nntpClient
	for i := 0; i < 3; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		cs = append(cs, c)
	}
	// 4th should block until release.
	done := make(chan *nntpClient, 1)
	go func() {
		c, err := pool.Acquire(ctx)
		if err == nil {
			done <- c
		}
	}()
	select {
	case <-done:
		t.Fatal("acquire succeeded beyond limit")
	case <-time.After(300 * time.Millisecond):
	}
	pool.Release(cs[0], false)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not unblock after release")
	}
	for _, c := range cs[1:] {
		pool.Release(c, false)
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
