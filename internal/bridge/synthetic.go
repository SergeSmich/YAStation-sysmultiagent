package bridge

import (
	"errors"
	"strings"
	"unicode/utf8"

	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"

	"quasarbridge/internal/wire"
)

// MaxSyntheticTextRunes bounds injected text so a stuck caller cannot flood a session.
const MaxSyntheticTextRunes = 500

// SyntheticTextInput builds a TextInput frame that can be injected into the live
// station session: the bridge routes it through Native.Client, so it takes the
// same path as a phrase typed on the device (routing, watchdog, local voice).
// Every call returns a fresh message id: Native.Client drops frames whose id is
// still pending.
func SyntheticTextInput(text string) (frame []byte, messageID string, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", errors.New("synthetic text is empty")
	}
	if utf8.RuneCountInString(text) > MaxSyntheticTextRunes {
		return nil, "", errors.New("synthetic text is too long")
	}
	// Application, DialogId and SequenceNumber are deliberately omitted: the
	// device identity is unknown here and a stale dialog sequence would order
	// the injected turn wrongly, so the cloud starts a fresh dialog instead.
	messageID = newID()
	message := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: messageID},
		Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{
			Header: &request.TAliceRequestProto_THeader{RequestId: new(messageID)},
			Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{
				Type: common.EEventType_text_input.Enum(),
				Text: new(text),
			}},
		}},
	}}}
	frame, err = (wire.Frame{Client: message, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return nil, "", err
	}
	return frame, messageID, nil
}
