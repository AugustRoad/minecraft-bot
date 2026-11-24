package minecraft

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	rconPacketAuth         int32 = 3
	rconPacketExecCommand  int32 = 2
	rconPacketAuthResponse int32 = 2
	rconPacketResponse     int32 = 0
	maxRCONPacketSize      int32 = 65536
)

var (
	// ErrRCONAuthFailed is returned when the Minecraft server rejects the RCON password.
	ErrRCONAuthFailed = errors.New("rcon authentication failed")
	// ErrRCONDisabled is returned when no RCON password is configured.
	ErrRCONDisabled = errors.New("rcon password not configured")
)

var rconReqID atomic.Int32

// RCONClient is a short-lived TCP client for Minecraft's Source RCON interface.
type RCONClient struct {
	conn net.Conn
}

// DialRCON connects and authenticates to a Minecraft RCON server.
func DialRCON(ctx context.Context, host string, port int, password string) (*RCONClient, error) {
	if host == "" {
		return nil, errors.New("rcon host is empty")
	}
	if password == "" {
		return nil, ErrRCONDisabled
	}
	if port <= 0 {
		port = 25575
	}

	dialer := net.Dialer{Timeout: 5 * time.Second}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial rcon %s: %w", addr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}

	client := &RCONClient{conn: conn}
	if err := client.authenticate(password); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

// Close terminates the underlying TCP connection.
func (c *RCONClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *RCONClient) authenticate(password string) error {
	reqID := nextRCONID()
	if err := c.writePacket(reqID, rconPacketAuth, password); err != nil {
		return fmt.Errorf("send rcon auth: %w", err)
	}

	// Some RCON servers send an empty RESPONSE_VALUE (0) packet before AUTH_RESPONSE (2).
	for i := 0; i < 2; i++ {
		respID, respType, _, err := c.readPacket()
		if err != nil {
			return fmt.Errorf("read rcon auth response: %w", err)
		}
		if respType == rconPacketAuthResponse {
			if respID == -1 || respID != reqID {
				return ErrRCONAuthFailed
			}
			return nil
		}
	}
	return ErrRCONAuthFailed
}

// Exec runs a single console command over RCON and returns the server's text response.
func (c *RCONClient) Exec(command string) (string, error) {
	reqID := nextRCONID()
	if err := c.writePacket(reqID, rconPacketExecCommand, command); err != nil {
		return "", fmt.Errorf("send rcon command: %w", err)
	}
	respID, _, body, err := c.readPacket()
	if err != nil {
		return "", fmt.Errorf("read rcon response: %w", err)
	}
	if respID != reqID {
		return "", fmt.Errorf("rcon response id mismatch: got %d want %d", respID, reqID)
	}
	return body, nil
}

func (c *RCONClient) writePacket(id, packetType int32, body string) error {
	// Size covers: id(4) + type(4) + body + two null terminators(2)
	size := int32(4 + 4 + len(body) + 2)
	var buf bytes.Buffer
	buf.Grow(int(size) + 4)

	_ = binary.Write(&buf, binary.LittleEndian, size)
	_ = binary.Write(&buf, binary.LittleEndian, id)
	_ = binary.Write(&buf, binary.LittleEndian, packetType)
	buf.WriteString(body)
	buf.Write([]byte{0x00, 0x00})

	_, err := c.conn.Write(buf.Bytes())
	return err
}

func (c *RCONClient) readPacket() (int32, int32, string, error) {
	var size int32
	if err := binary.Read(c.conn, binary.LittleEndian, &size); err != nil {
		return 0, 0, "", err
	}
	if size < 10 || size > maxRCONPacketSize {
		return 0, 0, "", fmt.Errorf("invalid rcon packet size: %d", size)
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return 0, 0, "", err
	}

	id := int32(binary.LittleEndian.Uint32(payload[0:4]))
	packetType := int32(binary.LittleEndian.Uint32(payload[4:8]))
	body := strings.TrimRight(string(payload[8:]), "\x00")
	return id, packetType, body, nil
}

func nextRCONID() int32 {
	id := rconReqID.Add(1)
	if id <= 0 {
		rconReqID.Store(1)
		return 1
	}
	return id
}

// Client implements ServerClient against a live Minecraft Java server via SLP and RCON.
type Client struct {
	MCPort       int
	RCONPort     int
	RCONPassword string
}

// ServerClient abstracts SLP and RCON interactions with the Minecraft server.
type ServerClient interface {
	QueryStatus(ctx context.Context, host string) (ServerStatus, error)
	SendDiscordChat(ctx context.Context, host, author, message string) error
	BroadcastNotice(ctx context.Context, host, notice string) error
	GracefulStop(ctx context.Context, host string) error
}

// QueryStatus queries live Minecraft server status over SLP.
func (c *Client) QueryStatus(ctx context.Context, host string) (ServerStatus, error) {
	return QueryStatus(ctx, host, c.MCPort)
}

// SendDiscordChat relays a Discord message to all online Minecraft players using safe JSON tellraw.
func (c *Client) SendDiscordChat(ctx context.Context, host, author, message string) error {
	cmd, err := BuildDiscordTellraw(author, message)
	if err != nil {
		return err
	}
	rcon, err := DialRCON(ctx, host, c.RCONPort, c.RCONPassword)
	if err != nil {
		return err
	}
	defer rcon.Close()
	_, err = rcon.Exec(cmd)
	return err
}

// BroadcastNotice sends a system announcement in gold text to all online Minecraft players.
func (c *Client) BroadcastNotice(ctx context.Context, host, notice string) error {
	cmd, err := BuildNoticeTellraw(notice)
	if err != nil {
		return err
	}
	rcon, err := DialRCON(ctx, host, c.RCONPort, c.RCONPassword)
	if err != nil {
		return err
	}
	defer rcon.Close()
	_, err = rcon.Exec(cmd)
	return err
}

// GracefulStop flushes world chunks to disk and stops the Minecraft server cleanly over RCON.
func (c *Client) GracefulStop(ctx context.Context, host string) error {
	rcon, err := DialRCON(ctx, host, c.RCONPort, c.RCONPassword)
	if err != nil {
		return err
	}
	defer rcon.Close()

	_, _ = rcon.Exec("say Server is shutting down now. Saving world...")
	if _, err := rcon.Exec("save-all flush"); err != nil {
		return fmt.Errorf("rcon save-all flush: %w", err)
	}
	if _, err := rcon.Exec("stop"); err != nil {
		return fmt.Errorf("rcon stop: %w", err)
	}
	return nil
}

type tellrawPart struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
}

// BuildDiscordTellraw constructs a command-injection-safe tellraw command for Discord messages.
func BuildDiscordTellraw(author, message string) (string, error) {
	cleanAuthor := sanitizeSingleLine(author, 48)
	cleanMessage := sanitizeSingleLine(message, 256)
	if cleanMessage == "" {
		return "", errors.New("empty chat message")
	}

	parts := []tellrawPart{
		{Text: "[Discord] ", Color: "aqua"},
		{Text: cleanAuthor, Color: "white"},
		{Text: ": " + cleanMessage, Color: "gray"},
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	return "tellraw @a " + string(raw), nil
}

// BuildNoticeTellraw constructs a command-injection-safe tellraw command for server notices.
func BuildNoticeTellraw(notice string) (string, error) {
	clean := sanitizeSingleLine(notice, 256)
	if clean == "" {
		return "", errors.New("empty notice")
	}
	parts := []tellrawPart{
		{Text: "[Server] ", Color: "gold"},
		{Text: clean, Color: "yellow"},
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	return "tellraw @a " + string(raw), nil
}

func sanitizeSingleLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)

	var out []rune
	for _, r := range s {
		if r < 32 || r == 127 {
			continue
		}
		out = append(out, r)
		if len(out) >= maxRunes {
			break
		}
	}
	return strings.TrimSpace(string(out))
}
