package events

import "time"

// EventType identifies the kind of event emitted by mc-agent to mc-bot.
type EventType string

const (
	TypeVMBoot         EventType = "vm_boot"
	TypeServerReady    EventType = "server_ready"
	TypeServerStopping EventType = "server_stopping"
	TypePlayerJoin     EventType = "player_join"
	TypePlayerLeave    EventType = "player_leave"
	TypeChat           EventType = "chat"
	TypeAdvancement    EventType = "advancement"
	TypeDeath          EventType = "death"
)

// Event is the JSON payload sent from mc-agent on mc-vm to mc-bot on bot-vm.
type Event struct {
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Player    string    `json:"player,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// Valid reports whether the event has a recognized Type.
func (e Event) Valid() bool {
	switch e.Type {
	case TypeVMBoot,
		TypeServerReady,
		TypeServerStopping,
		TypePlayerJoin,
		TypePlayerLeave,
		TypeChat,
		TypeAdvancement,
		TypeDeath:
		return true
	default:
		return false
	}
}
