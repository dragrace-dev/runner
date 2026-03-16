package nats

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"dragrace/internal/auth"
	"dragrace/internal/config"

	"github.com/nats-io/nats.go"
)

type Client struct {
	conn     *nats.Conn
	config   *config.Config
	clientID string // API client_id for NATS message auth headers
}

func NewClient(cfg *config.Config, credsFile string) (*Client, error) {
	opts := []nats.Option{
		nats.Name(cfg.RunnerID),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
	}

	// Use .creds file for NATS-level authentication
	if credsFile != "" {
		opts = append(opts, nats.UserCredentials(credsFile))
	}

	nc, err := nats.Connect(cfg.WsBackendURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	log.Printf("✅ Connected to NATS at %s", cfg.WsBackendURL)

	return &Client{
		conn:     nc,
		config:   cfg,
		clientID: auth.LoadClientID(),
	}, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Drain()
	}
}

// Subscribe to a topic
func (c *Client) Subscribe(subject string, handler func(*nats.Msg)) (*nats.Subscription, error) {
	return c.conn.Subscribe(subject, handler)
}

// newMsg creates a NATS message with auth headers attached.
func (c *Client) newMsg(subject string, data interface{}) (*nats.Msg, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message: %w", err)
	}

	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header:  nats.Header{},
	}

	if c.clientID != "" {
		msg.Header.Set("X-Runner-Client-ID", c.clientID)
	}

	return msg, nil
}

// Publish a message with auth headers.
func (c *Client) Publish(subject string, data interface{}) error {
	msg, err := c.newMsg(subject, data)
	if err != nil {
		return err
	}
	return c.conn.PublishMsg(msg)
}

// Send heartbeat
func (c *Client) SendHeartbeat(status string, currentJobID *string) error {
	heartbeat := map[string]interface{}{
		"runner_id": c.config.RunnerID,
		"status":    status,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	if currentJobID != nil {
		heartbeat["current_job_id"] = *currentJobID
	}

	return c.Publish("dragrace.dev.backend.runner.heartbeat", heartbeat)
}

// SendRunnerConfig sends complete hardware configuration to backend
func (c *Client) SendRunnerConfig(hwInfo interface{}) error {
	message := map[string]interface{}{
		"runner_id":       c.config.RunnerID,
		"hardware_config": hwInfo,
		"timestamp":       time.Now().Format(time.RFC3339),
	}

	return c.Publish("dragrace.dev.backend.runner.config", message)
}

// Request sends a request/reply message with auth headers.
func (c *Client) Request(subject string, data interface{}, timeout time.Duration) (*nats.Msg, error) {
	msg, err := c.newMsg(subject, data)
	if err != nil {
		return nil, err
	}
	return c.conn.RequestMsg(msg, timeout)
}

// RequestMsg sends a request with NATS headers support.
func (c *Client) RequestMsg(msg *nats.Msg, timeout time.Duration) (*nats.Msg, error) {
	return c.conn.RequestMsg(msg, timeout)
}

// Conn returns the underlying *nats.Conn.
func (c *Client) Conn() *nats.Conn {
	return c.conn
}
