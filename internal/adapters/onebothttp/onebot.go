// Package onebothttp is the OneBot v11 HTTP adapter: inbound webhooks with
// optional HMAC-SHA1 verification, outbound API calls.
package onebothttp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uniu-network/uniubot/internal/adapters"
	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/core/message"
	"github.com/uniu-network/uniubot/internal/core/roles"
)

const (
	adapterID   = "onebotHttp"
	adapterName = "OneBot HTTP"
)

func init() {
	adapters.Register(adapters.Meta{
		ID:   adapterID,
		Name: adapterName,
		DefaultConfig: map[string]any{
			"api_url":      "http://127.0.0.1:5700",
			"secret":       "",
			"access_token": "",
		},
	}, func(cfg map[string]any) adapters.Adapter {
		return New(cfg)
	})
}

type Adapter struct {
	cfg           map[string]any
	deps          adapters.Deps
	client        *http.Client
	legacyWarning sync.Once
}

func New(cfg map[string]any) *Adapter {
	return &Adapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) Name() string { return adapterName }

func (a *Adapter) optionalString(key string) string {
	if value, ok := a.cfg[key].(string); ok && value != "" {
		return value
	}
	return ""
}

// apiURL accepts the deprecated "url" alias for configs written before the
// api_url rename.
func (a *Adapter) apiURL() string {
	if value := a.optionalString("api_url"); value != "" {
		return value
	}
	if value := a.optionalString("url"); value != "" {
		a.legacyWarning.Do(func() {
			a.logger().Warn("onebotHttp: config key \"url\" is deprecated, use \"api_url\" instead")
		})
		return value
	}
	return ""
}

func (a *Adapter) secret() string      { return a.optionalString("secret") }
func (a *Adapter) accessToken() string { return a.optionalString("access_token") }
func (a *Adapter) logger() *logging.Logger {
	if a.deps.Logger != nil {
		return a.deps.Logger
	}
	return logging.Default()
}

func (a *Adapter) Start(_ context.Context, deps adapters.Deps) error {
	a.deps = deps
	a.logger().Info("OneBot HTTP adapter started, outbound API url: %s", a.apiURL())
	return nil
}

func (a *Adapter) Stop(context.Context) error {
	a.deps.Bot = nil
	return nil
}

func (a *Adapter) HandleHTTPRequest(_ context.Context, req *adapters.HTTPRequest) (*adapters.HTTPResponse, bool) {
	if a.deps.Bot == nil {
		return nil, false
	}
	if secret := a.secret(); secret != "" {
		signature := headerValue(req.Headers, "x-signature")
		if signature == "" {
			return &adapters.HTTPResponse{Status: 401, Body: map[string]any{"error": "missing signature"}}, true
		}
		if !verifySignature(req.RawBody, secret, signature) {
			return &adapters.HTTPResponse{Status: 403, Body: map[string]any{"error": "invalid signature"}}, true
		}
	}
	payload, ok := req.Body.(map[string]any)
	if !ok {
		return nil, false
	}
	event := matchEvent(payload, a)
	if event == nil {
		return nil, false
	}
	eventName := eventNameFor(payload)
	a.deps.Emit(eventName, event)
	return &adapters.HTTPResponse{Status: 200, Body: map[string]any{"status": "ok"}}, true
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func verifySignature(rawBody, secret, signature string) bool {
	if !strings.HasPrefix(signature, "sha1=") {
		return false
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(rawBody))
	expected := "sha1=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func (a *Adapter) SendGroup(ctx context.Context, targetID string, msg message.Message) error {
	return a.call(ctx, "/send_group_msg", map[string]any{
		"group_id": numericOrString(targetID),
		"message":  toOneBotMessage(msg),
	})
}

func (a *Adapter) SendPrivate(ctx context.Context, targetID string, msg message.Message) error {
	return a.call(ctx, "/send_private_msg", map[string]any{
		"user_id": numericOrString(targetID),
		"message": toOneBotMessage(msg),
	})
}

func numericOrString(value string) any {
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
		return parsed
	}
	return value
}

func (a *Adapter) call(ctx context.Context, path string, body map[string]any) error {
	base := strings.TrimSuffix(a.apiURL(), "/")
	if base == "" {
		return fmt.Errorf("onebotHttp: api_url is not configured")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token := a.accessToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("onebotHttp: %s returned HTTP %d: %s", path, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	return nil
}

func (a *Adapter) ApproveFriend(ctx context.Context, flag any, remark string) error {
	return a.call(ctx, "/set_friend_add_request", map[string]any{"flag": flag, "approve": true, "remark": remark})
}

func (a *Adapter) RejectFriend(ctx context.Context, flag any) error {
	return a.call(ctx, "/set_friend_add_request", map[string]any{"flag": flag, "approve": false})
}

func (a *Adapter) ApproveGroup(ctx context.Context, flag any, subType string) error {
	return a.call(ctx, "/set_group_add_request", map[string]any{"flag": flag, "sub_type": subType, "approve": true})
}

func (a *Adapter) RejectGroup(ctx context.Context, flag any, subType, reason string) error {
	return a.call(ctx, "/set_group_add_request", map[string]any{"flag": flag, "sub_type": subType, "approve": false, "reason": reason})
}

func (a *Adapter) SetGroupBan(ctx context.Context, groupID, userID int64, duration int) error {
	return a.call(ctx, "/set_group_ban", map[string]any{"group_id": groupID, "user_id": userID, "duration": duration})
}

func eventNameFor(payload map[string]any) string {
	postType := fmt.Sprint(payload["post_type"])
	switch postType {
	case "message", "notice", "request", "meta_event":
		return postType
	default:
		if postType == "" || postType == "<nil>" {
			postType = "unknown"
		}
		return adapterID + "." + postType
	}
}

// matchEvent returns nil when the payload cannot be represented.
func matchEvent(payload map[string]any, sender events.Sender) *events.Event {
	postType := stringValue(payload["post_type"])
	messageType := stringValue(payload["message_type"])
	noticeType := stringValue(payload["notice_type"])
	requestType := stringValue(payload["request_type"])
	metaEventType := stringValue(payload["meta_event_type"])
	subType := stringValue(payload["sub_type"])

	timeValue := int64Value(payload["time"])
	selfID := int64Value(payload["self_id"])

	switch postType {
	case "message":
		senderInfo := buildSender(payload["sender"])
		msg := parseMessage(payload["message"])
		rawMessage := stringValue(payload["raw_message"])
		messageID := payload["message_id"]
		switch messageType {
		case "group":
			return &events.Event{
				Time:       timeValue,
				SelfID:     selfID,
				AdapterID:  adapterID,
				Adapter:    sender,
				Kind:       events.KindGroupMessage,
				MessageID:  messageID,
				Message:    msg,
				RawMessage: rawMessage,
				Sender:     senderInfo,
				GroupID:    int64Value(payload["group_id"]),
				UserID:     int64Value(payload["user_id"]),
			}
		case "private":
			return &events.Event{
				Time:       timeValue,
				SelfID:     selfID,
				AdapterID:  adapterID,
				Adapter:    sender,
				Kind:       events.KindPrivateMessage,
				MessageID:  messageID,
				Message:    msg,
				RawMessage: rawMessage,
				Sender:     senderInfo,
				UserID:     int64Value(payload["user_id"]),
			}
		default:
			return nil
		}
	case "notice":
		return &events.Event{
			Time:       timeValue,
			SelfID:     selfID,
			AdapterID:  adapterID,
			Adapter:    sender,
			Kind:       events.KindNotice,
			NoticeType: noticeType,
			RawEvent:   marshalRaw(payload),
		}
	case "request":
		return &events.Event{
			Time:        timeValue,
			SelfID:      selfID,
			AdapterID:   adapterID,
			Adapter:     sender,
			Kind:        events.KindRequest,
			RequestType: requestType,
			RawEvent:    marshalRaw(payload),
		}
	case "meta_event":
		var status any
		if payload["status"] != nil {
			status = payload["status"]
		}
		return &events.Event{
			Time:          timeValue,
			SelfID:        selfID,
			AdapterID:     adapterID,
			Adapter:       sender,
			Kind:          events.KindMeta,
			MetaEventType: metaEventType,
			Status:        status,
		}
	default:
		detail := messageType
		if detail == "" {
			detail = noticeType
		}
		if detail == "" {
			detail = requestType
		}
		if detail == "" {
			detail = metaEventType
		}
		event := events.NewRaw(timeValue, selfID, adapterID, sender, postType, marshalRaw(payload))
		event.DetailType = detail
		event.SubType = subType
		return event
	}
}

func marshalRaw(payload map[string]any) json.RawMessage {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return encoded
}

func buildSender(raw any) events.UserInfo {
	out := events.UserInfo{}
	source, _ := raw.(map[string]any)
	if source == nil {
		return out
	}
	out.UserID = int64Value(source["user_id"])
	if value := stringValue(source["nickname"]); value != "" {
		out.Nickname = events.Str(value)
	}
	if value := stringValue(source["sex"]); value != "" {
		out.Sex = events.Str(value)
	}
	if value, ok := source["age"]; ok && value != nil {
		age := int(int64Value(value))
		out.Age = events.Int(age)
	}
	if value := stringValue(source["area"]); value != "" {
		out.Area = events.Str(value)
	}
	if value := stringValue(source["level"]); value != "" {
		out.Level = events.Str(value)
	}
	if value := stringValue(source["role"]); value != "" {
		out.Role = events.Str(value)
	}
	if value := stringValue(source["title"]); value != "" {
		out.Title = events.Str(value)
	}
	if value := stringValue(source["card"]); value != "" {
		out.Card = events.Str(value)
	}
	out.SkipRole = int(roles.FromOneBotRole(stringValue(source["role"])))
	return out
}

func parseMessage(raw any) message.Message {
	switch typed := raw.(type) {
	case []any:
		segments := make(message.Message, 0, len(typed))
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			segmentType := stringValue(entry["type"])
			if segmentType == "" {
				continue
			}
			data, _ := entry["data"].(map[string]any)
			if data == nil {
				data = map[string]any{}
			}
			segments = append(segments, message.Segment{Type: segmentType, Data: data})
		}
		return segments
	case string:
		// Some OneBot implementations post message_format=string: parse it as text.
		if typed == "" {
			return message.Message{}
		}
		return message.Text(typed)
	default:
		return message.Message{}
	}
}

func toOneBotMessage(msg message.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msg))
	for _, segment := range msg {
		switch segment.Type {
		case "raw":
			platform := stringValue(segment.Data["platform"])
			if platform != "onebot" {
				continue
			}
			data, _ := segment.Data["data"].(map[string]any)
			if data == nil {
				data = map[string]any{}
			}
			out = append(out, map[string]any{
				"type": stringValue(segment.Data["type"]),
				"data": data,
			})
		case "at":
			qq := segment.Data["qq"]
			if qq == nil {
				qq = segment.Data["user_id"]
			}
			out = append(out, map[string]any{
				"type": "at",
				"data": map[string]any{"qq": fmt.Sprint(qq)},
			})
		case "reply":
			out = append(out, map[string]any{
				"type": "reply",
				"data": map[string]any{"id": fmt.Sprint(segment.Data["id"])},
			})
		case "image":
			out = append(out, map[string]any{
				"type": "image",
				"data": map[string]any{"file": segment.Data["file"]},
			})
		default:
			out = append(out, map[string]any{"type": segment.Type, "data": segment.Data})
		}
	}
	return out
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	default:
		return 0
	}
}
