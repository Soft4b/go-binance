package futures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"

	"github.com/adshao/go-binance/v2/common"
)

//go:generate mockgen -source client_ws.go -destination mock/client_ws.go -package mock

const (
	// reconnectMinInterval define reconnect min interval
	reconnectMinInterval = 100 * time.Millisecond

	// reconnectMaxInterval define reconnect max interval
	reconnectMaxInterval = 10 * time.Second
)

var (
	// ErrorWsConnectionClosed defines that connection closed
	ErrorWsConnectionClosed = errors.New("ws error: connection closed")

	// ErrorWsReadConnectionTimeout defines that connection read timeout expired
	ErrorWsReadConnectionTimeout = errors.New("ws error: read connection timeout")

	// ErrorWsIdAlreadySent defines that request with the same id was already sent
	ErrorWsIdAlreadySent = errors.New("ws error: request with same id already sent")
)

// messageId define id field of request/response
type messageId struct {
	Id string `json:"id"`
}

// ClientWs define API websocket client
type ClientWs struct {
	APIKey                      string
	SecretKey                   string
	Debug                       bool
	KeyType                     string
	TimeOffset                  int64
	logger                      *log.Logger
	conn                        wsConnection
	connMu                      sync.Mutex
	reconnectSignal             chan struct{}
	connectionEstablishedSignal chan struct{}
	requestsList                RequestList
	readC                       chan []byte
	readErrChan                 chan error
	reconnectCount              atomic.Int64
}

func (c *ClientWs) debug(format string, v ...interface{}) {
	if c.Debug {
		c.logger.Println(fmt.Sprintf(format, v...))
	}
}

// NewClientWs init ClientWs
func NewClientWs(conn wsConnection, apiKey, secretKey string) (*ClientWs, error) {
	client := &ClientWs{
		APIKey:                      apiKey,
		SecretKey:                   secretKey,
		KeyType:                     common.KeyTypeHmac,
		logger:                      log.New(os.Stderr, "Binance-golang ", log.LstdFlags),
		conn:                        conn,
		connMu:                      sync.Mutex{},
		reconnectSignal:             make(chan struct{}, 1),
		connectionEstablishedSignal: make(chan struct{}, 1),
		requestsList:                NewRequestList(),
		readErrChan:                 make(chan error, 1),
		readC:                       make(chan []byte),
	}

	go client.handleReconnect()
	go client.read()

	return client, nil
}

type wsClient interface {
	Write(id string, data []byte) error
	WriteSync(id string, data []byte, timeout time.Duration) ([]byte, error)
	GetReadChannel() <-chan []byte
	GetReadErrorChannel() <-chan error
	GetApiKey() string
	GetSecretKey() string
	GetTimeOffset() int64
	GetKeyType() string
	GetReconnectCount() int64
	Wait(timeout time.Duration)
}

// Write sends data into websocket connection
func (c *ClientWs) Write(id string, data []byte) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.requestsList.IsAlreadyInList(id) {
		return ErrorWsIdAlreadySent
	}

	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.debug("write: unable to write message into websocket conn '%v'", err)
		return err
	}

	c.requestsList.Add(id)

	return nil
}

// WriteSync sends data to the websocket connection and waits for a response synchronously
// Should be used separately from the asynchronous Write method (do not send anything in parallel)
func (c *ClientWs) WriteSync(id string, data []byte, timeout time.Duration) ([]byte, error) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.debug("write sync: unable to write message into websocket conn '%v'", err)
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			c.debug("write sync: timeout expired")
			return nil, ErrorWsReadConnectionTimeout
		case rawData := <-c.readC:
			// check that the correct response from websocket has been read
			msg := messageId{}
			err := json.Unmarshal(rawData, &msg)
			if err != nil {
				return nil, err
			}
			if msg.Id != id {
				c.debug("write sync: wrong response with id '%v' has been read", msg.Id)
				continue
			}

			return rawData, nil
		case err := <-c.readErrChan:
			c.debug("write sync: error read '%v'", err)
			return nil, err
		}
	}
}

func (c *ClientWs) GetReadChannel() <-chan []byte {
	return c.readC
}

func (c *ClientWs) GetReadErrorChannel() <-chan error {
	return c.readErrChan
}

func (c *ClientWs) GetApiKey() string {
	return c.APIKey
}

func (c *ClientWs) GetSecretKey() string {
	return c.SecretKey
}

func (c *ClientWs) GetTimeOffset() int64 {
	return c.TimeOffset
}

func (c *ClientWs) GetKeyType() string {
	return c.KeyType
}

func (c *ClientWs) Wait(timeout time.Duration) {
	c.wait(timeout)
}

// read data from connection
func (c *ClientWs) read() {
	defer func() {
		// reading from closed connection 1000 times caused panic
		// prevent panic for any case
		if r := recover(); r != nil {
		}
	}()

	for {
		c.debug("read: waiting for message")
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			c.debug("read: error reading message '%v'", err)
			c.reconnectSignal <- struct{}{}
			c.readErrChan <- errors.Join(err, ErrorWsConnectionClosed)

			c.debug("read: wait to get connected")
			<-c.connectionEstablishedSignal

			// refresh map after reconnect to avoid useless waiting after stop application
			c.requestsList.RecreateList()

			c.debug("read: connection established")
			continue
		}
		c.debug("read: got new message")

		msg := messageId{}
		err = json.Unmarshal(message, &msg)
		if err != nil {
			c.debug("read: error unmarshalling message '%v'", err)
			c.readErrChan <- err
			continue
		}

		c.debug("read: sending message into read channel '%v'", msg)
		c.readC <- message

		c.debug("read: remove message from request list '%v'", msg)
		c.requestsList.Remove(msg.Id)
	}
}

// wait until all responses received
// make sure that you are not sending requests
func (c *ClientWs) wait(timeout time.Duration) {
	doneC := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if c.requestsList.Len() == 0 {
					doneC <- struct{}{}
					return
				}
			}
		}
	}()

	t := time.After(timeout)
	select {
	case <-t:
	case <-doneC:
	}

	cancel()
}

// handleReconnect waits for reconnect signal and starts reconnect
func (c *ClientWs) handleReconnect() {
	for _ = range c.reconnectSignal {
		c.debug("reconnect: received signal")

		b := &backoff.Backoff{
			Min:    reconnectMinInterval,
			Max:    reconnectMaxInterval,
			Factor: 1.8,
			Jitter: false,
		}

		conn := c.startReconnect(b)

		b.Reset()

		c.connMu.Lock()
		c.conn = conn
		c.connMu.Unlock()

		c.debug("reconnect: connected")
		c.connectionEstablishedSignal <- struct{}{}
	}
}

// startReconnect starts reconnect loop with increasing delay
func (c *ClientWs) startReconnect(b *backoff.Backoff) *connection {
	for {
		c.reconnectCount.Add(1)
		conn, err := newConnection()
		if err != nil {
			delay := b.Duration()
			c.debug("reconnect: error while reconnecting. try in %s", delay.Round(time.Millisecond))
			time.Sleep(delay)
			continue
		}

		return conn
	}
}

// GetReconnectCount returns reconnect counter value
func (c *ClientWs) GetReconnectCount() int64 {
	return c.reconnectCount.Load()
}

// NewRequestList creates request list
func NewRequestList() RequestList {
	return RequestList{
		mu:       sync.Mutex{},
		requests: make(map[string]struct{}), // TODO preallocate buckets
	}
}

// RequestList state of requests that was sent/received
type RequestList struct {
	mu       sync.Mutex
	requests map[string]struct{}
}

// Add adds request into list
func (l *RequestList) Add(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests[id] = struct{}{}
}

// RecreateList creates new request list
func (l *RequestList) RecreateList() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = make(map[string]struct{})
}

// Remove adds request from list
func (l *RequestList) Remove(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.requests, id)
}

// Len get list length
func (l *RequestList) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

// IsAlreadyInList checks if id is presented in requests list
func (l *RequestList) IsAlreadyInList(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.requests[id]; ok {
		return true
	}

	return false
}

// constructor for connection
func newConnection() (*connection, error) {
	conn, err := WsApiInitReadWriteConn()
	if err != nil {
		return nil, err
	}

	wsConn := &connection{
		conn:           conn,
		connectionMu:   sync.Mutex{},
		lastResponseMu: sync.Mutex{},
	}

	if WebsocketKeepalive {
		go wsConn.keepAlive(WebsocketTimeoutReadWriteConnection)
	}

	return wsConn, nil
}

// instance of single connection with keepalive handler
type connection struct {
	conn           *websocket.Conn
	connectionMu   sync.Mutex
	lastResponse   time.Time
	lastResponseMu sync.Mutex
}

type wsConnection interface {
	WriteMessage(messageType int, data []byte) error
	ReadMessage() (messageType int, p []byte, err error)
}

// WriteMessage is a thread-safe method for conn.WriteMessage
func (c *connection) WriteMessage(messageType int, data []byte) error {
	c.connectionMu.Lock()
	defer c.connectionMu.Unlock()
	return c.conn.WriteMessage(messageType, data)
}

// ReadMessage wrapper for conn.ReadMessage
func (c *connection) ReadMessage() (int, []byte, error) {
	return c.conn.ReadMessage()
}

// keepAlive handles ping-pong for connection
func (c *connection) keepAlive(timeout time.Duration) {
	ticker := time.NewTicker(timeout)

	c.updateLastResponse()

	c.conn.SetPongHandler(func(msg string) error {
		c.updateLastResponse()
		return nil
	})

	go func() {
		defer ticker.Stop()
		for {
			err := c.ping()
			if err != nil {
				return
			}

			<-ticker.C
			if c.isLastResponseOutdated(timeout) {
				c.close()
				return
			}
		}
	}()
}

// updateLastResponse sets lastResponse now
func (c *connection) updateLastResponse() {
	c.lastResponseMu.Lock()
	defer c.lastResponseMu.Unlock()
	c.lastResponse = time.Now()
}

// isLastResponseOutdated checks that time since last pong message exceeded timeout
func (c *connection) isLastResponseOutdated(timeout time.Duration) bool {
	c.lastResponseMu.Lock()
	defer c.lastResponseMu.Unlock()
	return time.Since(c.lastResponse) > timeout
}

// close thread-safe method for closing connection
func (c *connection) close() error {
	c.connectionMu.Lock()
	defer c.connectionMu.Unlock()
	return c.conn.Close()
}

// ping thread-safe method sending ping message
func (c *connection) ping() error {
	c.connectionMu.Lock()
	defer c.connectionMu.Unlock()

	deadline := time.Now().Add(10 * time.Second)
	err := c.conn.WriteControl(websocket.PingMessage, []byte{}, deadline)
	if err != nil {
		return err
	}

	return nil
}

// NewOrderPlaceWsService init OrderPlaceWsService
func NewOrderPlaceWsService(apiKey, secretKey string) (*OrderPlaceWsService, error) {
	conn, err := newConnection()
	if err != nil {
		return nil, err
	}

	client, err := NewClientWs(conn, apiKey, secretKey)
	if err != nil {
		return nil, err
	}

	return &OrderPlaceWsService{c: client}, nil
}

// NewOrderCancelWsService init OrderCancelWsService
func NewOrderCancelWsService(apiKey, secretKey string) (*OrderCancelWsService, error) {
	conn, err := newConnection()
	if err != nil {
		return nil, err
	}

	client, err := NewClientWs(conn, apiKey, secretKey)
	if err != nil {
		return nil, err
	}

	return &OrderCancelWsService{c: client}, nil
}