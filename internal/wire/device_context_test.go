package wire

import (
	"bytes"
	"testing"

	protos "github.com/freequasar/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func contextInput(kind string, withApplication bool) []byte {
	req := &request.TAliceRequestProto{
		Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_text_input.Enum()}},
	}
	if withApplication {
		req.Application = &protos.TClientInfoProto{DeviceId: new("device-1"), Lang: new("ru-RU")}
	}
	event := &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "context-test"}}
	switch kind {
	case "TextInput":
		event.Event = &client.TClientMessage_TEvent_TextInput{TextInput: req}
	case "VoiceInput":
		event.Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: req}
	case "SpeechInput":
		event.Event = &client.TClientMessage_TEvent_SpeechInput{SpeechInput: req}
	}
	data, err := proto.Marshal(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: event}})
	if err != nil {
		panic(err)
	}
	return append([]byte("AAPI"), data...)
}

func TestDeviceContextPatchesEveryInputKind(t *testing.T) {
	dc := DeviceContext{Lat: 55.7558, Lon: 37.6173, Accuracy: 1000, Timezone: "Europe/Moscow"}
	for _, kind := range []string{"TextInput", "VoiceInput", "SpeechInput"} {
		t.Run(kind, func(t *testing.T) {
			out, patch, err := AddDeviceContext(contextInput(kind, true), kind, dc)
			if err != nil {
				t.Fatal(err)
			}
			if !patch.Location || !patch.Timezone || patch.TimezoneSkipped {
				t.Fatalf("patch=%+v", patch)
			}
			if !bytes.HasPrefix(out, []byte("AAPI")) {
				t.Fatal("frame prefix lost")
			}
			f := Decode(out, ClientToServer)
			if f.Kind() != kind {
				t.Fatalf("kind=%s", f.Kind())
			}
			var req *request.TAliceRequestProto
			switch kind {
			case "TextInput":
				req = f.Client.GetEvent().GetTextInput()
			case "VoiceInput":
				req = f.Client.GetEvent().GetVoiceInput()
			case "SpeechInput":
				req = f.Client.GetEvent().GetSpeechInput()
			}
			loc := req.GetRequest().GetLocation()
			if loc.GetLat() != dc.Lat || loc.GetLon() != dc.Lon || loc.GetAccuracy() != dc.Accuracy {
				t.Fatalf("location=%v", loc)
			}
			if req.GetApplication().GetTimezone() != dc.Timezone {
				t.Fatalf("timezone=%q", req.GetApplication().GetTimezone())
			}
			if req.GetApplication().GetDeviceId() != "device-1" || req.GetApplication().GetLang() != "ru-RU" {
				t.Fatal("client info fields lost")
			}
		})
	}
}

func TestDeviceContextSkipsTimezoneWithoutApplication(t *testing.T) {
	out, patch, err := AddDeviceContext(contextInput("VoiceInput", false), "VoiceInput", DeviceContext{Lat: 55.7558, Lon: 37.6173, Timezone: "Europe/Moscow"})
	if err != nil {
		t.Fatal(err)
	}
	if !patch.Location || patch.Timezone || !patch.TimezoneSkipped {
		t.Fatalf("patch=%+v", patch)
	}
	req := Decode(out, ClientToServer).Client.GetEvent().GetVoiceInput()
	if req.GetRequest().GetLocation().GetLat() != 55.7558 {
		t.Fatal("location not written")
	}
	if req.GetApplication() != nil {
		t.Fatal("empty client info block was assembled")
	}
}

func TestDeviceContextKeepsUnknownAndBareFrames(t *testing.T) {
	dc := DeviceContext{Lat: 55.7558, Lon: 37.6173, Timezone: "Europe/Moscow"}
	raw := contextInput("TextInput", true)
	raw = protowire.AppendString(protowire.AppendTag(raw, 100, protowire.BytesType), "future-field")
	out, patch, err := AddDeviceContext(raw, "TextInput", dc)
	if err != nil {
		t.Fatal(err)
	}
	if !patch.Location || !patch.Timezone {
		t.Fatalf("patch=%+v", patch)
	}
	if !bytes.Contains(out, []byte("future-field")) {
		t.Fatal("unknown field dropped")
	}
	bare := out[4:]
	again, patch, err := AddDeviceContext(bare, "TextInput", dc)
	if err != nil {
		t.Fatal(err)
	}
	if !patch.Location || !patch.Timezone {
		t.Fatalf("bare patch=%+v", patch)
	}
	if bytes.HasPrefix(again, []byte("AAPI")) {
		t.Fatal("prefix invented for a bare frame")
	}
	req := Decode(again, ClientToServer).Client.GetEvent().GetTextInput()
	if req.GetRequest().GetLocation().GetLon() != 37.6173 || req.GetApplication().GetTimezone() != "Europe/Moscow" {
		t.Fatal("bare frame not patched")
	}
}

func TestDeviceContextDisabledAndUnsupported(t *testing.T) {
	raw := contextInput("TextInput", true)
	out, patch, err := AddDeviceContext(raw, "TextInput", DeviceContext{})
	if err != nil || patch != (ContextPatch{}) || !bytes.Equal(out, raw) {
		t.Fatalf("empty context changed the frame: %v %+v", err, patch)
	}
	if _, _, err := AddDeviceContext(raw, "ContinueRequest", DeviceContext{Lat: 1, Lon: 2}); err == nil {
		t.Fatal("non-input frame accepted")
	}
}
