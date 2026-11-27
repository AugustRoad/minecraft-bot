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
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	rconPacketAuth         int32 = 3
	rconPacketExecCommand  int32 = 2
	rconPacketAuthResponse int32 = 2
	rconPacketResponse     int32 = 0
	maxRCONPacketSize      int32 = 65536
	// Minecraft Java's RconClient uses a 1460-byte receive buffer (14-byte header + payload).
	maxRCONCommandBytes = 1400
	defaultRCONTimeout  = 10 * time.Second
)

var (
	// ErrRCONAuthFailed is returned when the Minecraft server rejects the RCON password.
	ErrRCONAuthFailed = errors.New("rcon authentication failed")
	// ErrRCONDisabled is returned when no RCON password is configured.
	ErrRCONDisabled = errors.New("rcon password not configured")
)

// RCONClient is a short-lived TCP client for Minecraft's Source RCON interface.
type RCONClient struct {
	conn       net.Conn
	stopCancel func() bool
	nextID     int32
}

// DialRCON connects and authenticates to a Minecraft RCON server.
func DialRCON(ctx context.Context, host string, port int, password string) (*RCONClient, error) {
	if host == "" {
		return nil, errors.New("rcon host is empty")
	}
	if password == "" {
		return nil, ErrRCONDisabled
	}
	if port == 0 {
		port = 25575
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid rcon port: %d", port)
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
		_ = conn.SetDeadline(time.Now().Add(defaultRCONTimeout))
	}
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})

	client := &RCONClient{
		conn:       conn,
		stopCancel: stopCancel,
	}
	if err := client.authenticate(password); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// Close terminates the underlying TCP connection and detaches context cancellation hooks.
func (c *RCONClient) Close() error {
	if c.stopCancel != nil {
		c.stopCancel()
		c.stopCancel = nil
	}
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *RCONClient) allocID() int32 {
	c.nextID++
	if c.nextID <= 0 {
		c.nextID = 1
	}
	return c.nextID
}

func (c *RCONClient) authenticate(password string) error {
	reqID := c.allocID()
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
	if len(command) > maxRCONCommandBytes {
		return "", fmt.Errorf("rcon command length %d exceeds max %d bytes", len(command), maxRCONCommandBytes)
	}
	reqID := c.allocID()
	if err := c.writePacket(reqID, rconPacketExecCommand, command); err != nil {
		return "", fmt.Errorf("send rcon command: %w", err)
	}
	respID, respType, body, err := c.readPacket()
	if err != nil {
		return "", fmt.Errorf("read rcon response: %w", err)
	}
	if respID != reqID {
		return "", fmt.Errorf("rcon response id mismatch: got %d want %d", respID, reqID)
	}
	if respType != rconPacketResponse {
		return "", fmt.Errorf("unexpected rcon response type: %d", respType)
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
	ExecCommand(ctx context.Context, host, command string) (string, error)
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

// ExecCommand executes a single Minecraft server console command over RCON and returns its output.
func (c *Client) ExecCommand(ctx context.Context, host, command string) (string, error) {
	clean := sanitizeSingleLine(strings.TrimPrefix(strings.TrimSpace(command), "/"), maxRCONCommandBytes)
	if clean == "" {
		return "", errors.New("command cannot be empty")
	}
	rcon, err := DialRCON(ctx, host, c.RCONPort, c.RCONPassword)
	if err != nil {
		return "", err
	}
	defer rcon.Close()

	out, err := rcon.Exec(clean)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stripFormatting(out)), nil
}

// GracefulStop flushes world chunks to disk and stops the Minecraft server cleanly over RCON.
// Some Java/Paper servers immediately close the RCON socket upon receiving "stop", so EOF/reset
// on the final stop command is treated as a successful shutdown initiation.
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
		if isExpectedStopDisconnect(err) {
			return nil
		}
		return fmt.Errorf("rcon stop: %w", err)
	}
	return nil
}

func isExpectedStopDisconnect(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET)
}

type tellrawPart struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
}

// BuildDiscordTellraw constructs a command-injection-safe tellraw command for Discord messages
// guaranteed to fit within Minecraft Java's RCON receive buffer.
func BuildDiscordTellraw(author, message string) (string, error) {
	cleanAuthor := sanitizeSingleLine(author, 48)
	cleanMessage := sanitizeSingleLine(message, 256)
	if cleanMessage == "" {
		return "", errors.New("empty chat message")
	}

	const prefix = "tellraw @a "
	for {
		parts := []tellrawPart{
			{Text: "[Discord] ", Color: "aqua"},
			{Text: cleanAuthor, Color: "white"},
			{Text: ": " + cleanMessage, Color: "gray"},
		}
		raw, err := marshalTellrawJSON(parts)
		if err != nil {
			return "", err
		}
		cmd := prefix + raw
		if len(cmd) <= maxRCONCommandBytes {
			return cmd, nil
		}
		// Trim message rune-by-rune until the encoded JSON fits safely under Minecraft's 1460B RCON buffer.
		runes := []rune(cleanMessage)
		if len(runes) <= 1 {
			return "", errors.New("discord tellraw command exceeds rcon buffer limit")
		}
		trimTo := len(runes) * 3 / 4
		if trimTo == len(runes) {
			trimTo = len(runes) - 1
		}
		cleanMessage = strings.TrimSpace(string(runes[:trimTo]))
		if cleanMessage == "" || !utf8.ValidString(cleanMessage) {
			return "", errors.New("empty chat message after truncation")
		}
	}
}

func marshalTellrawJSON(parts []tellrawPart) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(parts); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func sanitizeSingleLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)

	var out []rune
	for _, r := range s {
		if r < 32 || r == 127 || r == '§' {
			continue
		}
		out = append(out, r)
		if len(out) >= maxRunes {
			break
		}
	}
	return strings.TrimSpace(string(out))
}
