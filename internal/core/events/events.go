// Package events is the unified event model shared by adapters, the core and plugins.
// JSON field names are part of the API; the adapter is encoded as {"id","name"} only.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/uniu-network/uniubot/internal/core/message"
)

type Kind string

// Event kinds. The first five match the OneBot post types.
const (
	KindGroupMessage   Kind = "group_message"
	KindPrivateMessage Kind = "private_message"
	KindNotice         Kind = "notice"
	KindRequest        Kind = "request"
	KindMeta           Kind = "meta_event"
	KindBotSend        Kind = "bot_send"
	KindRaw            Kind = "raw"
)

type Sender interface {
	ID() string
	Name() string
	SendGroup(ctx context.Context, targetID string, msg message.Message) error
	SendPrivate(ctx context.Context, targetID string, msg message.Message) error
}

// UserInfo is the normalised sender; absent platform fields stay nil.
type UserInfo struct {
	UserID      int64   `json:"user_id"`
	Nickname    *string `json:"nickname,omitempty"`
	Sex         *string `json:"sex,omitempty"`
	Age         *int    `json:"age,omitempty"`
	Area        *string `json:"area,omitempty"`
	Level       *string `json:"level,omitempty"`
	Role        *string `json:"role,omitempty"`
	Title       *string `json:"title,omitempty"`
	Card        *string `json:"card,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	SessionID   *string `json:"session_id,omitempty"`
	// SkipRole is the SkiBot permission level (see internal/core/roles).
	SkipRole int `json:"ski_user_role"`
}

func Str(s string) *string { return &s }

func Int(i int) *int { return &i }

type Event struct {
	Time      int64
	SelfID    int64
	AdapterID string
	Adapter   Sender `json:"-"`
	Kind      Kind   `json:"-"`

	RawEvent json.RawMessage

	// Message payloads.
	MessageID  any             `json:"-"`
	Message    message.Message `json:"-"`
	RawMessage string          `json:"-"`
	Sender     UserInfo        `json:"-"`
	GroupID    int64           `json:"-"`
	UserID     int64           `json:"-"`

	NoticeType    string `json:"-"`
	RequestType   string `json:"-"`
	MetaEventType string `json:"-"`
	Status        any    `json:"-"`
	PostType      string `json:"-"`
	DetailType    string `json:"-"`
	SubType       string `json:"-"`

	SendMessageType string          `json:"-"`
	TargetID        int64           `json:"-"`
	SendMessage     message.Message `json:"-"`
}

func (e *Event) TypeName() string {
	switch e.Kind {
	case KindGroupMessage:
		return "GroupMessageEvent"
	case KindPrivateMessage:
		return "PrivateMessageEvent"
	case KindBotSend:
		return "BotSendEvent"
	case KindNotice:
		return "NoticeEvent"
	case KindRequest:
		return "RequestEvent"
	case KindMeta:
		return "MetaEvent"
	default:
		return "BotEvent"
	}
}

func (e *Event) IsMessage() bool {
	return e.Kind == KindGroupMessage || e.Kind == KindPrivateMessage
}

func (e *Event) MessageType() string {
	switch e.Kind {
	case KindGroupMessage:
		return "group"
	case KindPrivateMessage:
		return "private"
	default:
		return ""
	}
}

func (e *Event) Target() (messageType string, targetID int64) {
	switch e.Kind {
	case KindGroupMessage:
		return "group", e.GroupID
	case KindPrivateMessage:
		return "private", e.UserID
	default:
		return "", 0
	}
}

// Reply sends through the adapter and does not emit bot_send; the core does.
func (e *Event) Reply(ctx context.Context, msg message.Message) error {
	if e.Adapter == nil {
		return fmt.Errorf("event has no adapter")
	}
	messageType, targetID := e.Target()
	switch messageType {
	case "group":
		return e.Adapter.SendGroup(ctx, fmt.Sprint(targetID), msg)
	case "private":
		return e.Adapter.SendPrivate(ctx, fmt.Sprint(targetID), msg)
	default:
		return fmt.Errorf("event kind %s cannot be replied to", e.Kind)
	}
}

type field struct {
	key   string
	value any
}

func marshalObject(fields ...field) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(f.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		value, err := json.Marshal(f.value)
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (e *Event) adapterRef() map[string]any {
	if e.Adapter == nil {
		return nil
	}
	return map[string]any{"id": e.Adapter.ID(), "name": e.Adapter.Name()}
}

// MarshalJSON keeps the documented field order and names.
func (e *Event) MarshalJSON() ([]byte, error) {
	base := []field{
		{"time", e.Time},
		{"self_id", e.SelfID},
		{"adapter_id", e.AdapterID},
		{"adapter", e.adapterRef()},
	}
	appendRaw := func(fields []field) []field {
		if len(e.RawEvent) > 0 && string(e.RawEvent) != "null" {
			fields = append(fields, field{"raw_event", json.RawMessage(e.RawEvent)})
		}
		return fields
	}

	switch e.Kind {
	case KindGroupMessage:
		return marshalObject(append(base,
			field{"message_id", e.MessageID},
			field{"message", e.Message},
			field{"raw_message", e.RawMessage},
			field{"sender", e.Sender},
			field{"message_type", "group"},
			field{"group_id", e.GroupID},
			field{"user_id", e.UserID},
		)...)
	case KindPrivateMessage:
		return marshalObject(append(base,
			field{"message_id", e.MessageID},
			field{"message", e.Message},
			field{"raw_message", e.RawMessage},
			field{"sender", e.Sender},
			field{"message_type", "private"},
			field{"user_id", e.UserID},
		)...)
	case KindBotSend:
		return marshalObject(append(base,
			field{"message_type", e.SendMessageType},
			field{"target_id", e.TargetID},
			field{"message", e.SendMessage},
		)...)
	case KindNotice:
		return marshalObject(appendRaw(append(base,
			field{"notice_type", e.NoticeType},
		))...)
	case KindRequest:
		return marshalObject(appendRaw(append(base,
			field{"request_type", e.RequestType},
		))...)
	case KindMeta:
		fields := append(base, field{"meta_event_type", e.MetaEventType})
		if e.Status != nil {
			fields = append(fields, field{"status", e.Status})
		}
		return marshalObject(fields...)
	default:
		fields := append(base,
			field{"post_type", e.PostType},
			field{"detail_type", e.DetailType},
			field{"sub_type", e.SubType},
		)
		return marshalObject(appendRaw(fields)...)
	}
}

func (e *Event) Payload() (json.RawMessage, error) {
	encoded, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func NewRaw(time, selfID int64, adapterID string, adapter Sender, postType string, raw json.RawMessage) *Event {
	return &Event{
		Time:      time,
		SelfID:    selfID,
		AdapterID: adapterID,
		Adapter:   adapter,
		Kind:      KindRaw,
		PostType:  postType,
		RawEvent:  raw,
	}
}
