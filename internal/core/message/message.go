// Package message is the platform neutral message model: an ordered segment list.
// JSON: [{"type":"text","data":{"text":"hi"}}]
package message

import (
	"encoding/json"
	"fmt"
)

// Segment is one message element.
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

type Message []Segment

func Text(text string) Message {
	return Message{{Type: "text", Data: map[string]any{"text": text}}}
}

func At(userID any) Message {
	return Message{{Type: "at", Data: map[string]any{"user_id": fmt.Sprint(userID)}}}
}

func Reply(id any) Message {
	return Message{{Type: "reply", Data: map[string]any{"id": id}}}
}

func Image(file string) Message {
	return Message{{Type: "image", Data: map[string]any{"file": file}}}
}

func Raw(platform, segmentType string, data map[string]any) Message {
	if data == nil {
		data = map[string]any{}
	}
	return Message{{Type: "raw", Data: map[string]any{
		"platform": platform,
		"type":     segmentType,
		"data":     data,
	}}}
}

func New(segments ...Segment) Message {
	return Message(segments)
}

func FromJSON(raw []byte) (Message, error) {
	var segments []Segment
	if err := json.Unmarshal(raw, &segments); err != nil {
		return nil, err
	}
	return Message(segments), nil
}

// Add flattens nested messages.
func (m *Message) Add(segments ...Message) {
	for _, group := range segments {
		*m = append(*m, group...)
	}
}

func (m *Message) Push(segment Segment) {
	*m = append(*m, segment)
}

func (m Message) Clone() Message {
	out := make(Message, len(m))
	for i, segment := range m {
		data := make(map[string]any, len(segment.Data))
		for key, value := range segment.Data {
			data[key] = value
		}
		out[i] = Segment{Type: segment.Type, Data: data}
	}
	return out
}

// PlainText flattens text and at segments, as the WebChat adapter does.
func (m Message) PlainText() string {
	out := ""
	for _, segment := range m {
		switch segment.Type {
		case "text":
			if text, ok := segment.Data["text"]; ok {
				out += fmt.Sprint(text)
			}
		case "image":
			out += "[Image]"
		case "at":
			out += "@" + fmt.Sprint(segment.Data["user_id"])
		}
	}
	return out
}

func (m Message) TextContent() string {
	out := ""
	for _, segment := range m {
		if segment.Type == "text" {
			out += fmt.Sprint(segment.Data["text"])
		} else if segment.Type == "at" {
			out += "@" + fmt.Sprint(segment.Data["user_id"])
		}
	}
	return out
}

func (m Message) IsEmpty() bool { return len(m) == 0 }
