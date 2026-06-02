// Package wsclient e' un sottile wrapper sulla connessione WebSocket verso il
// gateway, con invio serializzato (un solo writer per volta).
package wsclient

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

// Dial apre una connessione WSS al gateway.
func Dial(rawURL string) (*Conn, error) {
	d := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	ws, _, err := d.Dial(rawURL, nil)
	if err != nil {
		return nil, err
	}
	return &Conn{ws: ws}, nil
}

// WriteJSON serializza gli invii con un mutex.
func (c *Conn) WriteJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteJSON(v)
}

// ReadJSON legge un frame. Va chiamato da una sola goroutine (il read loop).
func (c *Conn) ReadJSON(v any) error {
	return c.ws.ReadJSON(v)
}

func (c *Conn) Close() error {
	return c.ws.Close()
}
