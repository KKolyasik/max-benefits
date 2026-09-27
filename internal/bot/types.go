package bot

import "context"

// EventType is what the user did.
type EventType int

const (
	// EventStart: the user opened the bot for the first time.
	EventStart EventType = iota + 1
	// EventText: the user sent a text message.
	EventText
	// EventCallback: the user pressed an inline button.
	EventCallback
)

// Event is a transport-independent user action.
type Event struct {
	Type EventType
	// UserID identifies the user; replies go to the dialog with them.
	UserID int64
	Text   string
	// Callback fields.
	CallbackID string
	Payload    string
	// SourceText is the plain text of the message whose button was pressed.
	// It is empty if the message is gone.
	SourceText string
}

// Message is an outgoing message.
type Message struct {
	Text string
	// Markdown enables **bold**, _italic_ and [links](https://...).
	Markdown bool
	Keyboard [][]Button
	// Silent delivers the message without a sound.
	Silent bool
}

// Button is a callback button when Payload is set and a link button when URL
// is set.
type Button struct {
	Text    string
	Payload string
	URL     string
}

// CallbackAnswer acknowledges a button press.
type CallbackAnswer struct {
	// Edit, if set, replaces the message with the pressed button.
	Edit *Message
	// Notification is a short one-time toast.
	Notification string
}

// Messenger delivers messages to users. The MAX adapter implements it; tests
// and the console simulator use their own implementations.
type Messenger interface {
	Send(ctx context.Context, userID int64, msg Message) error
	// AnswerCallback acknowledges a button press of the user.
	AnswerCallback(ctx context.Context, userID int64, callbackID string, answer CallbackAnswer) error
}
