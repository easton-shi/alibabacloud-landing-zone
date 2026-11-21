// Package ws wraps the executor WebSocket connection.
package ws

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client is a concurrency-safe WebSocket writer with a single reader.
type Client struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// Dial opens /ws/executor with executorId + token in the query string.
func Dial(ctx context.Context, endpoint string) (*Client, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, response, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("dial executor websocket: HTTP %d: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("dial executor websocket: %w", err)
	}
	conn.SetReadLimit(16 * 1024 * 1024)
	return &Client{conn: conn}, nil
}

// Send writes one JSON frame.
func (c *Client) Send(value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.conn.WriteJSON(value)
}

// Read returns the next text frame.
func (c *Client) Read() ([]byte, error) {
	_, payload, err := c.conn.ReadMessage()
	return payload, err
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }
